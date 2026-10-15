//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
)

// identityCacheRepoStub 是可在测试中途切换取值与错误的设置仓库桩。blockNext 之后的下一次读取在取得值或错误之后
// 先发出"已阻塞"信号、再等放行，等待期间不持锁，其他读取照常进行。
type identityCacheRepoStub struct {
	SettingRepository

	mu      sync.Mutex
	values  map[string]string
	errKeys map[string]error
	calls   int
	blocked chan struct{}
	release chan struct{}
}

func newIdentityCacheRepoStub(values map[string]string) *identityCacheRepoStub {
	return &identityCacheRepoStub{values: values, errKeys: map[string]error{}}
}

func (r *identityCacheRepoStub) read(keys []string) (map[string]string, error) {
	r.mu.Lock()
	r.calls++
	var err error
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if keyErr := r.errKeys[key]; keyErr != nil {
			err = keyErr
		}
		out[key] = r.values[key]
	}
	blocked, release := r.blocked, r.release
	r.blocked, r.release = nil, nil
	r.mu.Unlock()
	if blocked != nil {
		close(blocked)
		<-release
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *identityCacheRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	return r.read(keys)
}

func (r *identityCacheRepoStub) GetValue(_ context.Context, key string) (string, error) {
	out, err := r.read([]string{key})
	if err != nil {
		return "", err
	}
	return out[key], nil
}

func (r *identityCacheRepoStub) set(key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
}

func (r *identityCacheRepoStub) setErr(key string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.errKeys, key)
		return
	}
	r.errKeys[key] = err
}

func (r *identityCacheRepoStub) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// blockNext 让下一次读取阻塞，返回"已阻塞"信号与放行开关。
func (r *identityCacheRepoStub) blockNext() (<-chan struct{}, chan<- struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blocked = make(chan struct{})
	r.release = make(chan struct{})
	return r.blocked, r.release
}

// identityCacheCase 描述一个出站身份读取：它读哪个设置键、内置值是什么、怎么读缓存条目。
type identityCacheCase struct {
	name     string
	key      string
	value    string
	newValue string
	fallback string
	get      func(s *SettingService) string
	cache    func(s *SettingService) *atomic.Value
	// entry 返回缓存条目的值与过期时间（unix nano）；ok 为 false 表示缓存里没有条目。
	entry func(s *SettingService) (value string, expiresAt int64, ok bool)
	// expire 把当前条目换成同值、已过期的新条目，模拟 60 秒 TTL 到期。
	expire func(s *SettingService)
	// save 模拟保存设置时的直写（只有 Codex UA 与 Antigravity 版本有），nil 表示没有。
	save func(s *SettingService, value string)
	// invalidate 是失效函数（只有两个版本号缓存有），nil 表示没有。
	invalidate func(s *SettingService)
}

func identityCacheCases() []identityCacheCase {
	ctx := context.Background()
	return []identityCacheCase{
		{
			name:     "Codex 版本号",
			key:      SettingKeyOpenAICodexClientVersionSynced,
			value:    "0.162.1",
			newValue: "0.163.0",
			fallback: codexCLIVersion,
			get:      func(s *SettingService) string { return s.GetOpenAICodexClientVersion(ctx) },
			cache:    func(s *SettingService) *atomic.Value { return &s.openAICodexVersionCache },
			entry: func(s *SettingService) (string, int64, bool) {
				e, _ := s.openAICodexVersionCache.Load().(*cachedOpenAICodexClientVersion)
				if e == nil {
					return "", 0, false
				}
				return e.version, e.expiresAt, true
			},
			expire: func(s *SettingService) {
				e := s.openAICodexVersionCache.Load().(*cachedOpenAICodexClientVersion)
				s.openAICodexVersionCache.Store(&cachedOpenAICodexClientVersion{version: e.version})
			},
			invalidate: func(s *SettingService) { s.InvalidateOpenAICodexClientVersionCache() },
		},
		{
			name:     "Codex User-Agent",
			key:      SettingKeyOpenAICodexUserAgent,
			value:    "codex_cli_rs/0.150.0 (Mac OS 15.1.0; arm64) iTerm.app",
			newValue: "codex-tui/0.150.0 (Ubuntu 24.4.0; x86_64) xterm-256color",
			fallback: DefaultOpenAICodexUserAgent,
			get:      func(s *SettingService) string { return s.GetOpenAICodexUserAgent(ctx) },
			cache:    func(s *SettingService) *atomic.Value { return &s.openAICodexUACache },
			entry: func(s *SettingService) (string, int64, bool) {
				e, _ := s.openAICodexUACache.Load().(*cachedOpenAICodexUserAgent)
				if e == nil {
					return "", 0, false
				}
				return e.value, e.expiresAt, true
			},
			expire: func(s *SettingService) {
				e := s.openAICodexUACache.Load().(*cachedOpenAICodexUserAgent)
				s.openAICodexUACache.Store(&cachedOpenAICodexUserAgent{value: e.value})
			},
			save: func(s *SettingService, value string) {
				s.openAICodexUASF.Forget("openai_codex_user_agent")
				s.openAICodexUACache.Store(&cachedOpenAICodexUserAgent{
					value:     value,
					expiresAt: time.Now().Add(openAICodexUserAgentCacheTTL).UnixNano(),
				})
			},
		},
		{
			name:     "Claude Code 版本号",
			key:      SettingKeyClaudeCodeClientVersionSynced,
			value:    "99.0.0",
			newValue: "99.0.1",
			fallback: claude.CLIVersion(),
			get:      func(s *SettingService) string { return s.GetClaudeCodeClientVersion(ctx) },
			cache:    func(s *SettingService) *atomic.Value { return &s.claudeCodeVersionCache },
			entry: func(s *SettingService) (string, int64, bool) {
				e, _ := s.claudeCodeVersionCache.Load().(*cachedClaudeCodeClientVersion)
				if e == nil {
					return "", 0, false
				}
				return e.version, e.expiresAt, true
			},
			expire: func(s *SettingService) {
				e := s.claudeCodeVersionCache.Load().(*cachedClaudeCodeClientVersion)
				s.claudeCodeVersionCache.Store(&cachedClaudeCodeClientVersion{version: e.version})
			},
			invalidate: func(s *SettingService) { s.InvalidateClaudeCodeClientVersionCache() },
		},
		{
			name:     "Antigravity 版本号",
			key:      SettingKeyAntigravityUserAgentVersion,
			value:    "1.99.0",
			newValue: "1.99.1",
			fallback: antigravity.GetDefaultUserAgentVersion(),
			get:      func(s *SettingService) string { return s.GetAntigravityUserAgentVersion(ctx) },
			cache:    func(s *SettingService) *atomic.Value { return &s.antigravityUAVersionCache },
			entry: func(s *SettingService) (string, int64, bool) {
				e, _ := s.antigravityUAVersionCache.Load().(*cachedAntigravityUserAgentVersion)
				if e == nil {
					return "", 0, false
				}
				return e.version, e.expiresAt, true
			},
			expire: func(s *SettingService) {
				e := s.antigravityUAVersionCache.Load().(*cachedAntigravityUserAgentVersion)
				s.antigravityUAVersionCache.Store(&cachedAntigravityUserAgentVersion{version: e.version})
			},
			save: func(s *SettingService, value string) {
				s.antigravityUAVersionSF.Forget("antigravity_user_agent_version")
				s.antigravityUAVersionCache.Store(&cachedAntigravityUserAgentVersion{
					version:   value,
					expiresAt: time.Now().Add(antigravityUserAgentVersionCacheTTL).UnixNano(),
				})
			},
		},
	}
}

var errIdentityCacheDBDown = errors.New("db down")

// requireEntryExpiresWithin 断言缓存条目还在有效期内，且剩余时间不超过 ttl。
func requireEntryExpiresWithin(t *testing.T, tc identityCacheCase, s *SettingService, ttl time.Duration) {
	t.Helper()
	_, expiresAt, ok := tc.entry(s)
	require.True(t, ok)
	now := time.Now()
	require.Greater(t, expiresAt, now.UnixNano())
	require.LessOrEqual(t, expiresAt, now.Add(ttl).UnixNano())
}

func requireEntryExpired(t *testing.T, tc identityCacheCase, s *SettingService) {
	t.Helper()
	_, expiresAt, ok := tc.entry(s)
	require.True(t, ok)
	require.LessOrEqual(t, expiresAt, time.Now().UnixNano())
}

// 读过一次之后数据库出错：沿用上一次的值、按错误 TTL 缓存，期间不再回源；恢复并过期后读到新值。
func TestClientIdentityKeepsLastValueOnReadError(t *testing.T) {
	for _, tc := range identityCacheCases() {
		t.Run(tc.name, func(t *testing.T) {
			repo := newIdentityCacheRepoStub(map[string]string{tc.key: tc.value})
			svc := NewSettingService(repo, nil)
			require.Equal(t, tc.value, tc.get(svc))

			tc.expire(svc)
			repo.setErr(tc.key, errIdentityCacheDBDown)
			require.Equal(t, tc.value, tc.get(svc))
			requireEntryExpiresWithin(t, tc, svc, 5*time.Second)

			calls := repo.callCount()
			require.Equal(t, tc.value, tc.get(svc))
			require.Equal(t, calls, repo.callCount(), "错误 TTL 内不应再回源")

			repo.setErr(tc.key, nil)
			repo.set(tc.key, tc.newValue)
			tc.expire(svc)
			require.Equal(t, tc.newValue, tc.get(svc))
		})
	}
}

// 零值缓存的首次读取：成功与失败都能发布，随后命中缓存、不再回源；从没读成功过时失败仍返回内置值。
func TestClientIdentityFirstReadPublishes(t *testing.T) {
	for _, tc := range identityCacheCases() {
		t.Run(tc.name+"/成功", func(t *testing.T) {
			repo := newIdentityCacheRepoStub(map[string]string{tc.key: tc.value})
			svc := NewSettingService(repo, nil)
			require.Equal(t, tc.value, tc.get(svc))
			require.Equal(t, tc.value, tc.get(svc))
			require.Equal(t, 1, repo.callCount())
		})
		t.Run(tc.name+"/失败", func(t *testing.T) {
			repo := newIdentityCacheRepoStub(map[string]string{tc.key: tc.value})
			repo.setErr(tc.key, errIdentityCacheDBDown)
			svc := NewSettingService(repo, nil)
			require.Equal(t, tc.fallback, tc.get(svc))
			require.Equal(t, tc.fallback, tc.get(svc))
			require.Equal(t, 1, repo.callCount())
		})
	}
}

// 失效之后读：成功就是新值；失败则沿用失效前的值，失效前没有值时是内置值。
func TestClientVersionInvalidateKeepsValueForReadError(t *testing.T) {
	for _, tc := range identityCacheCases() {
		if tc.invalidate == nil {
			continue
		}
		t.Run(tc.name+"/读成功", func(t *testing.T) {
			repo := newIdentityCacheRepoStub(map[string]string{tc.key: tc.value})
			svc := NewSettingService(repo, nil)
			require.Equal(t, tc.value, tc.get(svc))
			repo.set(tc.key, tc.newValue)
			tc.invalidate(svc)
			require.Equal(t, tc.newValue, tc.get(svc))
		})
		t.Run(tc.name+"/读失败", func(t *testing.T) {
			repo := newIdentityCacheRepoStub(map[string]string{tc.key: tc.value})
			svc := NewSettingService(repo, nil)
			require.Equal(t, tc.value, tc.get(svc))
			tc.invalidate(svc)
			repo.setErr(tc.key, errIdentityCacheDBDown)
			require.Equal(t, tc.value, tc.get(svc))
		})
		t.Run(tc.name+"/空缓存读失败", func(t *testing.T) {
			repo := newIdentityCacheRepoStub(map[string]string{tc.key: tc.value})
			repo.setErr(tc.key, errIdentityCacheDBDown)
			svc := NewSettingService(repo, nil)
			tc.invalidate(svc)
			require.Equal(t, tc.fallback, tc.get(svc))
		})
	}
}

// startBlockedRead 让下一次回源阻塞，并在另一个协程里发起读取；返回读取结果与放行开关。
func startBlockedRead(t *testing.T, tc identityCacheCase, repo *identityCacheRepoStub, svc *SettingService) (<-chan string, chan<- struct{}) {
	t.Helper()
	blocked, release := repo.blockNext()
	result := make(chan string, 1)
	go func() { result <- tc.get(svc) }()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("回源没有开始")
	}
	return result, release
}

func awaitBlockedRead(t *testing.T, result <-chan string) string {
	t.Helper()
	select {
	case v := <-result:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("被放行的回源没有返回")
		return ""
	}
}

// 保存直写发生在一次回源进行中：回源返回后不覆盖直写的新值，调用方拿到的也是新值。
func TestClientIdentityStaleReadDoesNotOverwriteSave(t *testing.T) {
	for _, tc := range identityCacheCases() {
		if tc.save == nil {
			continue
		}
		for _, failing := range []bool{false, true} {
			name := tc.name + "/旧回源成功"
			if failing {
				name = tc.name + "/旧回源出错"
			}
			t.Run(name, func(t *testing.T) {
				repo := newIdentityCacheRepoStub(map[string]string{tc.key: tc.value})
				svc := NewSettingService(repo, nil)
				require.Equal(t, tc.value, tc.get(svc))
				tc.expire(svc)
				if failing {
					repo.setErr(tc.key, errIdentityCacheDBDown)
				}

				result, release := startBlockedRead(t, tc, repo, svc)
				tc.save(svc, tc.newValue)
				close(release)

				require.Equal(t, tc.newValue, awaitBlockedRead(t, result))
				got, _, _ := tc.entry(svc)
				require.Equal(t, tc.newValue, got)
				requireEntryExpiresWithin(t, tc, svc, time.Minute)
			})
		}
	}
}

// 失效之后紧接着有一次新的回源发布了新值：之前开始的旧回源返回后不覆盖它，调用方拿到新值。
func TestClientVersionStaleReadDoesNotOverwriteRefreshAfterInvalidate(t *testing.T) {
	for _, tc := range identityCacheCases() {
		if tc.invalidate == nil {
			continue
		}
		for _, failing := range []bool{false, true} {
			name := tc.name + "/旧回源成功"
			if failing {
				name = tc.name + "/旧回源出错"
			}
			t.Run(name, func(t *testing.T) {
				repo := newIdentityCacheRepoStub(map[string]string{tc.key: tc.value})
				svc := NewSettingService(repo, nil)
				require.Equal(t, tc.value, tc.get(svc))
				tc.expire(svc)
				if failing {
					repo.setErr(tc.key, errIdentityCacheDBDown)
				}

				result, release := startBlockedRead(t, tc, repo, svc)
				repo.setErr(tc.key, nil)
				repo.set(tc.key, tc.newValue)
				tc.invalidate(svc)
				require.Equal(t, tc.newValue, tc.get(svc))
				close(release)

				require.Equal(t, tc.newValue, awaitBlockedRead(t, result))
				got, _, _ := tc.entry(svc)
				require.Equal(t, tc.newValue, got)
				requireEntryExpiresWithin(t, tc, svc, time.Minute)
			})
		}
	}
}

// 只有失效、没有新的回源：旧回源返回后不给失效条目续上有效期，下一次读取照常回源拿到新值。
func TestClientVersionStaleReadKeepsInvalidationExpired(t *testing.T) {
	for _, tc := range identityCacheCases() {
		if tc.invalidate == nil {
			continue
		}
		for _, failing := range []bool{false, true} {
			name := tc.name + "/旧回源成功"
			if failing {
				name = tc.name + "/旧回源出错"
			}
			t.Run(name, func(t *testing.T) {
				repo := newIdentityCacheRepoStub(map[string]string{tc.key: tc.value})
				svc := NewSettingService(repo, nil)
				require.Equal(t, tc.value, tc.get(svc))
				tc.expire(svc)
				if failing {
					repo.setErr(tc.key, errIdentityCacheDBDown)
				}

				result, release := startBlockedRead(t, tc, repo, svc)
				repo.setErr(tc.key, nil)
				repo.set(tc.key, tc.newValue)
				tc.invalidate(svc)
				close(release)

				require.Equal(t, tc.value, awaitBlockedRead(t, result), "返回失效条目里失效前的值")
				requireEntryExpired(t, tc, svc)
				calls := repo.callCount()
				require.Equal(t, tc.newValue, tc.get(svc))
				require.Equal(t, calls+1, repo.callCount())
			})
		}
	}
}

// 缓存从没读成功过时连续失效两次：两次失效写入的是不同的新条目，期间阻塞的旧回源同样发布不出去。
func TestClientVersionStaleReadAfterRepeatedInvalidateOnEmptyCache(t *testing.T) {
	for _, tc := range identityCacheCases() {
		if tc.invalidate == nil {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			repo := newIdentityCacheRepoStub(map[string]string{tc.key: tc.value})
			svc := NewSettingService(repo, nil)
			tc.invalidate(svc)

			result, release := startBlockedRead(t, tc, repo, svc)
			repo.set(tc.key, tc.newValue)
			tc.invalidate(svc)
			close(release)

			require.Equal(t, tc.fallback, awaitBlockedRead(t, result))
			requireEntryExpired(t, tc, svc)
			require.Equal(t, tc.newValue, tc.get(svc))
		})
	}
}

// 身份链路：预热后版本号或 UA 读不到（单独或同时），收口后的 UA 首段、UA 版本段与 version 头仍是预热的版本；
// 账号级陈旧 UA 同样被重建为该版本。
//
// 不得给本用例加 t.Parallel()：它改写进程级解析器。
func TestCodexIdentityKeepsWarmVersionWhenSettingsReadFails(t *testing.T) {
	const panelUA = "codex_cli_rs/0.150.0 (Mac OS 15.1.0; arm64) iTerm.app"
	for _, tt := range []struct {
		name       string
		failingKey []string
	}{
		{name: "版本号读不到", failingKey: []string{SettingKeyOpenAICodexClientVersionSynced}},
		{name: "UA 读不到", failingKey: []string{SettingKeyOpenAICodexUserAgent}},
		{name: "两者都读不到", failingKey: []string{SettingKeyOpenAICodexClientVersionSynced, SettingKeyOpenAICodexUserAgent}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := newIdentityCacheRepoStub(map[string]string{
				SettingKeyOpenAICodexClientVersionSynced: "0.162.1",
				SettingKeyOpenAICodexUserAgent:           panelUA,
			})
			svc := NewSettingService(repo, nil)
			SetCodexCanonicalUserAgentResolver(func() string {
				return svc.GetOpenAICodexCanonicalUserAgent(context.Background())
			})
			t.Cleanup(func() { SetCodexCanonicalUserAgentResolver(nil) })

			enforce := func(overrideUA string) http.Header {
				h := make(http.Header)
				h.Set("originator", "codex_cli_rs")
				enforceCodexIdentityHeadersWithUA(h, overrideUA)
				return h
			}
			warm := enforce("")
			require.Equal(t, "codex_cli_rs/0.162.1 (Mac OS 15.1.0; arm64) iTerm.app", warm.Get("user-agent"))
			require.Equal(t, "0.162.1", warm.Get("version"))

			for _, c := range identityCacheCases()[:2] {
				c.expire(svc)
			}
			for _, key := range tt.failingKey {
				repo.setErr(key, errIdentityCacheDBDown)
			}

			h := enforce("")
			require.Equal(t, "codex_cli_rs", h.Get("originator"))
			require.Equal(t, warm.Get("user-agent"), h.Get("user-agent"))
			require.Equal(t, "0.162.1", h.Get("version"))

			stale := enforce("codex-tui/0.120.0 (Windows 10.0.19045; x86_64) WindowsTerminal")
			require.Equal(t, "codex-tui", stale.Get("originator"))
			require.Equal(t, "codex-tui/0.162.1 (Windows 10.0.19045; x86_64) WindowsTerminal", stale.Get("user-agent"))
			require.Equal(t, "0.162.1", stale.Get("version"))
		})
	}
}
