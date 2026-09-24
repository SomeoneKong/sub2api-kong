package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// KongPaceConfigFileName 是账号消耗节奏的配置文件名，放在上游的数据目录（DATA_DIR）。
//
// 不走环境变量：上线后要按运行情况调参，改环境变量要重建容器、会掐断进行中的流式响应；
// 配置文件每拍检查一次修改时间，改后一分钟内生效，enabled: false 同时是应急开关。
// 文件名带平台名，将来其他平台的同类控制各用各的文件。
const KongPaceConfigFileName = "openai-account-pace.yaml"

// KongPaceConfigPath 返回数据目录下的配置文件路径。
func KongPaceConfigPath(dataDir string) string {
	return filepath.Join(dataDir, KongPaceConfigFileName)
}

// KongPaceConfig 是解析、校验之后的生效配置。字段含义见 DESIGN-openai-account-pace.md 第 6 节。
type KongPaceConfig struct {
	Enabled       bool                    `json:"enabled"`
	GapDoublingPP float64                 `json:"gap_doubling_pp"`
	GapCapPP      float64                 `json:"gap_cap_pp"`
	MaxPenaltyPP  float64                 `json:"max_penalty_pp"`
	Plans         map[string]KongPacePlan `json:"plans"`
	DefaultPlan   string                  `json:"default_plan"`
	Concurrency   KongPaceConcurrency     `json:"concurrency"`
	Sessions      KongPaceSessions        `json:"sessions"`
	DecisionLog   KongPaceDecisionLog     `json:"decision_log"`
}

// KongPacePlan 是单个套餐的参数。
type KongPacePlan struct {
	Capacity float64           `json:"capacity"`
	YieldPP  float64           `json:"yield_pp"`
	SoftLine KongPaceSoftLines `json:"soft_line_percent"`
}

// KongPaceSoftLines 是 6h / 24h 两个窗口的软线。
type KongPaceSoftLines struct {
	H6  KongPaceSoftLine `json:"6h"`
	H24 KongPaceSoftLine `json:"24h"`
}

// KongPaceSoftLine 是一个窗口的起罚线与软线，单位是占本账号周额度的百分点。
type KongPaceSoftLine struct {
	Start float64 `json:"start"`
	Full  float64 `json:"full"`
}

// KongPaceConcurrency 是并发占用调整的参数。Enabled 为 false 时调整恒为 0，峰值采样照常。
type KongPaceConcurrency struct {
	Enabled           bool                       `json:"enabled"`
	PeakWindowMinutes int                        `json:"peak_window_minutes"`
	IdleBonusPP       float64                    `json:"idle_bonus_pp"`
	BusyRatio         float64                    `json:"busy_ratio"`
	PenaltyPP         KongPaceConcurrencyPenalty `json:"penalty_pp"`
}

// KongPaceConcurrencyPenalty 是占用超过 busy_ratio 之后按剩余槽数的三档惩罚。
type KongPaceConcurrencyPenalty struct {
	Free2Plus float64 `json:"free_2_plus"`
	Free1     float64 `json:"free_1"`
	Full      float64 `json:"full"`
}

// KongPaceSessions 是会话占用调整的参数：MaxPenaltyPP 是占用为 1 时的扣分，按占用比例线性。
// Enabled 为 false 时调整恒为 0，不影响会话上限的分段。
type KongPaceSessions struct {
	Enabled      bool    `json:"enabled"`
	MaxPenaltyPP float64 `json:"max_penalty_pp"`
}

// KongPaceDecisionLog 是决策记录的参数。
type KongPaceDecisionLog struct {
	Enabled       bool `json:"enabled"`
	RetentionDays int  `json:"retention_days"`
}

// plan 返回套餐参数；表外套餐与未知套餐用默认套餐。
func (c *KongPaceConfig) plan(name string) KongPacePlan {
	if p, ok := c.Plans[name]; ok {
		return p
	}
	return c.Plans[c.DefaultPlan]
}

// 文件结构全部用指针：yaml 解码分不出"没写"与"写了零值"，而本配置要求全部字段必填——
// 半截的文件不能被悄悄补上默认值。
type kongPaceConfigFile struct {
	Enabled       *bool                        `yaml:"enabled"`
	GapDoublingPP *float64                     `yaml:"gap_doubling_pp"`
	GapCapPP      *float64                     `yaml:"gap_cap_pp"`
	MaxPenaltyPP  *float64                     `yaml:"max_penalty_pp"`
	Plans         map[string]*kongPacePlanFile `yaml:"plans"`
	DefaultPlan   *string                      `yaml:"default_plan"`
	Concurrency   *kongPaceConcurrencyFile     `yaml:"concurrency"`
	Sessions      *kongPaceSessionsFile        `yaml:"sessions"`
	DecisionLog   *kongPaceDecisionLogFile     `yaml:"decision_log"`
}

type kongPacePlanFile struct {
	Capacity *float64               `yaml:"capacity"`
	YieldPP  *float64               `yaml:"yield_pp"`
	SoftLine *kongPaceSoftLinesFile `yaml:"soft_line_percent"`
}

type kongPaceSoftLinesFile struct {
	H6  *kongPaceSoftLineFile `yaml:"6h"`
	H24 *kongPaceSoftLineFile `yaml:"24h"`
}

type kongPaceSoftLineFile struct {
	Start *float64 `yaml:"start"`
	Full  *float64 `yaml:"full"`
}

type kongPaceConcurrencyFile struct {
	Enabled           *bool                           `yaml:"enabled"`
	PeakWindowMinutes *int                            `yaml:"peak_window_minutes"`
	IdleBonusPP       *float64                        `yaml:"idle_bonus_pp"`
	BusyRatio         *float64                        `yaml:"busy_ratio"`
	PenaltyPP         *kongPaceConcurrencyPenaltyFile `yaml:"penalty_pp"`
}

type kongPaceConcurrencyPenaltyFile struct {
	Free2Plus *float64 `yaml:"free_2_plus"`
	Free1     *float64 `yaml:"free_1"`
	Full      *float64 `yaml:"full"`
}

type kongPaceSessionsFile struct {
	Enabled      *bool    `yaml:"enabled"`
	MaxPenaltyPP *float64 `yaml:"max_penalty_pp"`
}

type kongPaceDecisionLogFile struct {
	Enabled       *bool `yaml:"enabled"`
	RetentionDays *int  `yaml:"retention_days"`
}

// kongPaceConfigErrors 收集校验错误，一次报全，免得改一处报一处。
type kongPaceConfigErrors struct{ errs []error }

func (e *kongPaceConfigErrors) addf(format string, args ...any) {
	e.errs = append(e.errs, fmt.Errorf(format, args...))
}

func (e *kongPaceConfigErrors) err() error { return errors.Join(e.errs...) }

func kongPaceNeedFloat(e *kongPaceConfigErrors, v *float64, name string, ok func(float64) bool, rule string) float64 {
	if v == nil {
		e.addf("%s: 缺失", name)
		return 0
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) || !ok(*v) {
		e.addf("%s: %v 不满足 %s", name, *v, rule)
	}
	return *v
}

func kongPaceNeedInt(e *kongPaceConfigErrors, v *int, name string, ok func(int) bool, rule string) int {
	if v == nil {
		e.addf("%s: 缺失", name)
		return 0
	}
	if !ok(*v) {
		e.addf("%s: %v 不满足 %s", name, *v, rule)
	}
	return *v
}

func kongPaceNeedBool(e *kongPaceConfigErrors, v *bool, name string) bool {
	if v == nil {
		e.addf("%s: 缺失", name)
		return false
	}
	return *v
}

// ParseKongPaceConfig 解析并校验配置文件内容。出现未知字段即非法：拼错的键不会被静默忽略。
func ParseKongPaceConfig(data []byte) (KongPaceConfig, error) {
	var f kongPaceConfigFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return KongPaceConfig{}, fmt.Errorf("解析失败: %w", err)
	}
	// 后面若还有文档（---），其内容不会被校验却会算进文件版本，一律拒收。
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return KongPaceConfig{}, fmt.Errorf("解析失败: %w", err)
		}
		return KongPaceConfig{}, errors.New("解析失败: 文件里只能有一个 YAML 文档")
	}

	e := &kongPaceConfigErrors{}
	positive := func(v float64) bool { return v > 0 }
	nonNegative := func(v float64) bool { return v >= 0 }
	cfg := KongPaceConfig{
		Enabled:       kongPaceNeedBool(e, f.Enabled, "enabled"),
		GapDoublingPP: kongPaceNeedFloat(e, f.GapDoublingPP, "gap_doubling_pp", positive, "> 0"),
		GapCapPP:      kongPaceNeedFloat(e, f.GapCapPP, "gap_cap_pp", positive, "> 0"),
		MaxPenaltyPP:  kongPaceNeedFloat(e, f.MaxPenaltyPP, "max_penalty_pp", nonNegative, ">= 0"),
		Plans:         map[string]KongPacePlan{},
	}

	if len(f.Plans) == 0 {
		e.addf("plans: 至少要有一个套餐")
	}
	names := make([]string, 0, len(f.Plans))
	for name := range f.Plans {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pf := f.Plans[name]
		prefix := "plans." + name
		if pf == nil {
			e.addf("%s: 缺失", prefix)
			continue
		}
		p := KongPacePlan{
			Capacity: kongPaceNeedFloat(e, pf.Capacity, prefix+".capacity", func(v float64) bool { return v > 0 && v <= kongPaceMaxCapacity }, "0 < x <= 100"),
			YieldPP:  kongPaceNeedFloat(e, pf.YieldPP, prefix+".yield_pp", nonNegative, ">= 0"),
		}
		if pf.SoftLine == nil {
			e.addf("%s.soft_line_percent: 缺失", prefix)
		} else {
			p.SoftLine.H6 = kongPaceSoftLineOf(e, pf.SoftLine.H6, prefix+".soft_line_percent.6h")
			p.SoftLine.H24 = kongPaceSoftLineOf(e, pf.SoftLine.H24, prefix+".soft_line_percent.24h")
		}
		cfg.Plans[name] = p
	}

	if f.DefaultPlan == nil {
		e.addf("default_plan: 缺失")
	} else {
		cfg.DefaultPlan = *f.DefaultPlan
		if _, ok := f.Plans[cfg.DefaultPlan]; !ok {
			e.addf("default_plan: %q 不在 plans 里", cfg.DefaultPlan)
		}
	}

	if f.Concurrency == nil {
		e.addf("concurrency: 缺失")
	} else {
		c := f.Concurrency
		cfg.Concurrency = KongPaceConcurrency{
			Enabled:           kongPaceNeedBool(e, c.Enabled, "concurrency.enabled"),
			PeakWindowMinutes: kongPaceNeedInt(e, c.PeakWindowMinutes, "concurrency.peak_window_minutes", func(v int) bool { return v >= 1 && v <= kongPacePeakKeep }, "1 <= x <= 60"),
			IdleBonusPP:       kongPaceNeedFloat(e, c.IdleBonusPP, "concurrency.idle_bonus_pp", nonNegative, ">= 0"),
			BusyRatio:         kongPaceNeedFloat(e, c.BusyRatio, "concurrency.busy_ratio", func(v float64) bool { return v > 0 && v < 1 }, "0 < x < 1"),
		}
		if c.PenaltyPP == nil {
			e.addf("concurrency.penalty_pp: 缺失")
		} else {
			pp := KongPaceConcurrencyPenalty{
				Free2Plus: kongPaceNeedFloat(e, c.PenaltyPP.Free2Plus, "concurrency.penalty_pp.free_2_plus", nonNegative, ">= 0"),
				Free1:     kongPaceNeedFloat(e, c.PenaltyPP.Free1, "concurrency.penalty_pp.free_1", nonNegative, ">= 0"),
				Full:      kongPaceNeedFloat(e, c.PenaltyPP.Full, "concurrency.penalty_pp.full", nonNegative, ">= 0"),
			}
			if pp.Free2Plus > pp.Free1 || pp.Free1 > pp.Full {
				e.addf("concurrency.penalty_pp: 需满足 free_2_plus <= free_1 <= full")
			}
			cfg.Concurrency.PenaltyPP = pp
		}
	}

	if f.Sessions == nil {
		e.addf("sessions: 缺失")
	} else {
		cfg.Sessions = KongPaceSessions{
			Enabled:      kongPaceNeedBool(e, f.Sessions.Enabled, "sessions.enabled"),
			MaxPenaltyPP: kongPaceNeedFloat(e, f.Sessions.MaxPenaltyPP, "sessions.max_penalty_pp", nonNegative, ">= 0"),
		}
	}

	if f.DecisionLog == nil {
		e.addf("decision_log: 缺失")
	} else {
		cfg.DecisionLog = KongPaceDecisionLog{
			Enabled:       kongPaceNeedBool(e, f.DecisionLog.Enabled, "decision_log.enabled"),
			RetentionDays: kongPaceNeedInt(e, f.DecisionLog.RetentionDays, "decision_log.retention_days", func(v int) bool { return v >= 1 }, ">= 1"),
		}
	}

	// 权重最大是 capacity × 2^((gap_cap_pp + idle_bonus_pp) ÷ gap_doubling_pp)。指数与容量都有上限，
	// 权重及其总和就不会溢出成 Inf、让加权抽样失效。
	if cfg.GapDoublingPP > 0 && (cfg.GapCapPP+cfg.Concurrency.IdleBonusPP)/cfg.GapDoublingPP > kongPaceMaxExponent {
		e.addf("gap_doubling_pp: 需满足 (gap_cap_pp + concurrency.idle_bonus_pp) / gap_doubling_pp <= %d，否则权重会溢出", kongPaceMaxExponent)
	}

	if err := e.err(); err != nil {
		return KongPaceConfig{}, err
	}
	return cfg, nil
}

const (
	kongPaceMaxCapacity = 100
	kongPaceMaxExponent = 64
)

func kongPaceSoftLineOf(e *kongPaceConfigErrors, f *kongPaceSoftLineFile, name string) KongPaceSoftLine {
	if f == nil {
		e.addf("%s: 缺失", name)
		return KongPaceSoftLine{}
	}
	l := KongPaceSoftLine{
		Start: kongPaceNeedFloat(e, f.Start, name+".start", func(v float64) bool { return v >= 0 }, ">= 0"),
		Full:  kongPaceNeedFloat(e, f.Full, name+".full", func(v float64) bool { return v > 0 && v <= 100 }, "0 < x <= 100"),
	}
	if l.Start >= l.Full {
		e.addf("%s: 需满足 start < full", name)
	}
	return l
}

// kongPaceLoadedConfig 是一份生效配置及其来源信息。
type kongPaceLoadedConfig struct {
	cfg      KongPaceConfig
	version  string // 文件内容的 sha256 前 12 位
	loadedAt time.Time
	json     []byte // cfg 的 JSON，写进每条决策记录
}

// kongPaceConfigSource 按修改时间热加载配置文件。
//
// 规则（设计文档第 6 节）：文件不存在即功能关闭；启动时非法同样关闭（不阻止网关启动）；
// 运行中改坏沿用上一份有效配置；运行中删除视同不存在。
type kongPaceConfigSource struct {
	path     string
	readFile func(string) ([]byte, error)
	stat     func(string) (os.FileInfo, error)

	mu        sync.Mutex
	current   *kongPaceLoadedConfig
	modTime   time.Time
	size      int64
	lastError string
}

func newKongPaceConfigSource(path string) *kongPaceConfigSource {
	return &kongPaceConfigSource{path: path, readFile: os.ReadFile, stat: os.Stat}
}

// kongPaceConfigEvent 描述一次检查的结果，供调用方打日志。
type kongPaceConfigEvent int

const (
	kongPaceConfigUnchanged kongPaceConfigEvent = iota
	kongPaceConfigLoaded
	kongPaceConfigRemoved
	kongPaceConfigInvalid
)

// check 检查文件并在需要时重新加载，返回检查后的生效配置（nil 表示功能关闭）。
func (s *kongPaceConfigSource) check(now time.Time) (*kongPaceLoadedConfig, kongPaceConfigEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := s.stat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if s.current == nil && s.modTime.IsZero() {
				s.lastError = ""
				return nil, kongPaceConfigUnchanged, nil
			}
			s.current, s.modTime, s.size, s.lastError = nil, time.Time{}, 0, ""
			return nil, kongPaceConfigRemoved, nil
		}
		return s.current, kongPaceConfigInvalid, fmt.Errorf("读取配置文件状态失败: %w", err)
	}
	if info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return s.current, kongPaceConfigUnchanged, nil
	}

	// 读不出来（如权限不对）不记修改时间：修好权限不会改变修改时间与大小，下一拍要能重试。
	data, err := s.readFile(s.path)
	if err != nil {
		s.lastError = err.Error()
		return s.current, kongPaceConfigInvalid, fmt.Errorf("读取配置文件失败: %w", err)
	}
	// 读到内容就记下修改时间：改坏的文件只报一次，不必每拍重复解析同一份坏内容。
	s.modTime, s.size = info.ModTime(), info.Size()
	cfg, err := ParseKongPaceConfig(data)
	if err != nil {
		s.lastError = err.Error()
		return s.current, kongPaceConfigInvalid, err
	}
	sum := sha256.Sum256(data)
	encoded, _ := json.Marshal(cfg)
	s.current = &kongPaceLoadedConfig{cfg: cfg, version: hex.EncodeToString(sum[:])[:12], loadedAt: now, json: encoded}
	s.lastError = ""
	return s.current, kongPaceConfigLoaded, nil
}

// status 返回当前生效配置与最近一次加载错误。
func (s *kongPaceConfigSource) status() (*kongPaceLoadedConfig, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, s.lastError
}
