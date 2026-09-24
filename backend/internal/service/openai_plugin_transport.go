package service

import "net/http"

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// doOpenAIUpstream 是全部 OpenAI **HTTP** 上游发送的汇聚点，codex 票据的准入与交付判定挂在这里。
//
// [kong] 挂在这一层而不是各业务分支里：二十来个调用点都经过它，逐个去插漏一条就等于开一个
// 降智出口。**原生 WebSocket 的帧不经过这里**，那两条路各自在帧出入口接入同一套判定（见
// kong_ticket_gateway.go 的文件头与 WS 段）。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	attempt, err := s.kongTicket.PrepareUpstream(request.Context(), request, account)
	if err != nil {
		return nil, err
	}
	// [kong] 把本次发送的特征记录器挂到出站请求上，调用方据它组装用量行（见
	// kong_ticket_request_feature.go）。就地换 context 而不是返回一个新请求：调用方持有的就是这个
	// 指针，换掉整个对象会让它手里那份失去记录器，而发送尚未开始，此刻没有并发读者。
	if attempt != nil && attempt.Features != nil {
		*request = *request.WithContext(kongWithFeatures(request.Context(), attempt.Features))
	}
	// [kong] 发往官方 ChatGPT 后端的请求体按 zstd 压缩（见 kong_openai_request_zstd.go）。
	response, err := s.kongSendOpenAIUpstreamCompressed(request, proxyURL, account)
	if err != nil {
		return response, err
	}
	if guardErr := s.kongTicket.AfterUpstream(request.Context(), account, attempt, response); guardErr != nil {
		// 响应头已到、正文还没交给调用方，直接关闭即「零业务正文交付」。
		//
		// **不排空**：SSE 流要等整轮生成才 EOF，排空会让拒服一直挂到上游写完——而我们已经从
		// 响应头确定这次输出不受保障，那些字节没有任何用途。关闭连接同时也让上游停止生成。
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, guardErr
	}
	return response, nil
}

// sendOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) sendOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}
