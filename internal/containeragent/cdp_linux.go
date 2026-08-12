//go:build linux

package containeragent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

func RunCDPProxy(ctx context.Context, listen, upstreamUnix string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host != "127.0.0.1" {
		return fmt.Errorf("CDP proxy must listen on IPv4 loopback")
	}
	if !filepath.IsAbs(upstreamUnix) {
		return fmt.Errorf("CDP upstream Unix socket must be absolute")
	}
	dial := func(dialContext context.Context) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(dialContext, "unix", upstreamUnix)
	}
	transport := &http.Transport{DisableCompression: true, DialContext: func(dialContext context.Context, _, _ string) (net.Conn, error) {
		return dial(dialContext)
	}}
	proxy := cdpProxy{client: &http.Client{Transport: transport, Timeout: 30 * time.Second}, dial: dial}
	listener, err := net.Listen("tcp4", listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

type cdpProxy struct {
	client *http.Client
	dial   func(context.Context) (net.Conn, error)
}

func (p cdpProxy) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if request.Header.Get("Upgrade") != "" {
		p.proxyWebSocket(response, request)
		return
	}
	target := &url.URL{Scheme: "http", Host: "cohotfs-host", Path: request.URL.Path, RawQuery: request.URL.RawQuery}
	upstreamRequest, err := http.NewRequestWithContext(request.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadGateway)
		return
	}
	upstreamResponse, err := p.client.Do(upstreamRequest)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstreamResponse.Body.Close()
	body, err := io.ReadAll(io.LimitReader(upstreamResponse.Body, 16<<20))
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadGateway)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/json") && strings.Contains(upstreamResponse.Header.Get("Content-Type"), "application/json") {
		body = rewriteWebSocketURLs(body, request.Host)
	}
	for key, values := range upstreamResponse.Header {
		for _, value := range values {
			response.Header().Add(key, value)
		}
	}
	response.Header().Set("Content-Length", fmt.Sprint(len(body)))
	response.WriteHeader(upstreamResponse.StatusCode)
	_, _ = response.Write(body)
}

func rewriteWebSocketURLs(body []byte, host string) []byte {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return body
	}
	rewriteJSONStrings(value, host)
	result, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return result
}

func rewriteJSONStrings(value any, host string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if key == "webSocketDebuggerUrl" {
				if raw, ok := item.(string); ok {
					if parsed, err := url.Parse(raw); err == nil && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost") {
						parsed.Host = host
						typed[key] = parsed.String()
					}
				}
				continue
			}
			rewriteJSONStrings(item, host)
		}
	case []any:
		for _, item := range typed {
			rewriteJSONStrings(item, host)
		}
	}
}

func (p cdpProxy) proxyWebSocket(response http.ResponseWriter, request *http.Request) {
	hijacker, ok := response.(http.Hijacker)
	if !ok {
		http.Error(response, "hijacking unavailable", http.StatusInternalServerError)
		return
	}
	upstream, err := p.dial(request.Context())
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadGateway)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	defer client.Close()
	defer upstream.Close()
	request.URL.Scheme, request.URL.Host = "", ""
	request.Host = "127.0.0.1"
	if err := request.Write(upstream); err != nil {
		return
	}
	if buffered.Reader.Buffered() != 0 {
		if _, err := io.CopyN(upstream, buffered, int64(buffered.Reader.Buffered())); err != nil {
			return
		}
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}
