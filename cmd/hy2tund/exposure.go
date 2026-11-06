package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"hy2tun/internal/config"
	"hy2tun/internal/view"
)

const (
	exposureDefaultConfig = "server.yaml"
	exposureTimeout       = 15 * time.Second
	exposureMaxBody       = 1 << 20
)

type exposureOptions struct {
	clientID   string
	mappingID  string
	mode       view.Exposure
	configPath string
	insecure   bool
}

// runExposure implements the `hy2tund exposure` subcommand. It deliberately
// loads the server configuration without starting the server or generating a
// Web password. The password is read from stdin and never enters argv, the
// environment, or log output.
func runExposure(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	opts, err := parseExposureArgs(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}

	cfg, err := config.LoadServer(opts.configPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Web.Listen) == "" {
		return fmt.Errorf("Web 控制台未启用：%s 中 web.listen 为空", opts.configPath)
	}

	useTLS := cfg.Web.TLS.Cert != "" && cfg.Web.TLS.Key != ""
	if opts.insecure && !useTLS {
		return errors.New("--insecure 仅适用于启用了 HTTPS 的 Web 控制台")
	}
	targets, err := resolveWebTargets(cfg.Web.Listen, useTLS)
	if err != nil {
		return err
	}

	username := cfg.Web.Username
	if username == "" {
		username = "admin"
	}

	password, err := readExposurePassword(stdin, stderr)
	if err != nil {
		return err
	}

	client, err := newExposureHTTPClient(opts.insecure)
	if err != nil {
		clear(password)
		return err
	}
	baseURL, loginErr := loginExposureTargets(client, targets, username, password, useTLS)
	// The password is only needed for the login request. Clear the byte slice
	// before the exposure API call and before any error is returned.
	clear(password)
	if loginErr != nil {
		return loginErr
	}
	return applyExposure(client, baseURL, opts, stdout, stderr)
}

func parseExposureArgs(args []string, stderr io.Writer) (exposureOptions, error) {
	var (
		opts exposureOptions
		mode string
	)
	fs := flag.NewFlagSet("exposure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.clientID, "client", "", "客户端 ID")
	fs.StringVar(&opts.mappingID, "mapping", "", "映射 ID")
	fs.StringVar(&mode, "mode", "", "暴露范围：public 或 local")
	fs.StringVar(&opts.configPath, "c", exposureDefaultConfig, "服务端配置文件路径")
	fs.BoolVar(&opts.insecure, "insecure", false, "跳过 Web 控制台 TLS 证书校验（仅用于自签名证书）")
	if err := fs.Parse(args); err != nil {
		return exposureOptions{}, err
	}
	if fs.NArg() > 0 {
		return exposureOptions{}, fmt.Errorf("exposure: 未识别的参数：%s", strings.Join(fs.Args(), " "))
	}

	if strings.TrimSpace(opts.clientID) == "" {
		return exposureOptions{}, errors.New("exposure: 必须指定 --client")
	}
	if strings.TrimSpace(opts.mappingID) == "" {
		return exposureOptions{}, errors.New("exposure: 必须指定 --mapping")
	}
	if strings.TrimSpace(opts.configPath) == "" {
		return exposureOptions{}, errors.New("exposure: 配置文件路径不能为空")
	}
	opts.mode = view.Exposure(strings.ToLower(strings.TrimSpace(mode)))
	if !opts.mode.Valid() {
		return exposureOptions{}, errors.New("exposure: --mode 必须是 public 或 local")
	}
	return opts, nil
}

// resolveWebTargets turns web.listen into the URL candidates the CLI should
// try. Wildcard binds are reached over loopback: IPv4 first, then IPv6. A
// specific address or hostname is used exactly as configured.
func resolveWebTargets(listen string, useTLS bool) ([]string, error) {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return nil, errors.New("Web 控制台未启用：web.listen 为空")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("web.listen 无效（%q）：%w", listen, err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("web.listen 端口无效（%q）", port)
	}
	port = strconv.Itoa(portNumber)

	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	if host == "" || isWildcardHost(host) {
		return []string{
			scheme + "://" + net.JoinHostPort("127.0.0.1", port),
			scheme + "://" + net.JoinHostPort("::1", port),
		}, nil
	}
	return []string{scheme + "://" + net.JoinHostPort(host, port)}, nil
}

func isWildcardHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsUnspecified()
}

func newExposureHTTPClient(insecure bool) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("创建会话 Cookie 存储失败：%w", err)
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	transport := base.Clone()
	if insecure {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		} else {
			transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		}
		// This is the explicit --insecure opt-in required for self-signed
		// Web certificates. Normal verification stays on by default.
		transport.TLSClientConfig.InsecureSkipVerify = true
	}

	return &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   exposureTimeout,
		// Login and API responses are expected to be direct JSON responses.
		// Do not silently follow a redirect to an unrelated page.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func readExposurePassword(stdin io.Reader, stderr io.Writer) ([]byte, error) {
	if stdin == nil {
		return nil, errors.New("未提供 Web 管理员密码输入")
	}
	_, _ = fmt.Fprint(stderr, "Web 管理员密码: ")

	if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		password, err := term.ReadPassword(int(file.Fd()))
		_, _ = fmt.Fprintln(stderr)
		if err != nil {
			clear(password)
			return nil, fmt.Errorf("读取 Web 管理员密码失败：%w", err)
		}
		if len(password) == 0 {
			return nil, errors.New("未输入 Web 管理员密码")
		}
		return password, nil
	}

	reader := bufio.NewReader(stdin)
	line, err := reader.ReadBytes('\n')
	_, _ = fmt.Fprintln(stderr)
	if err != nil && !errors.Is(err, io.EOF) {
		clear(line)
		return nil, fmt.Errorf("读取 Web 管理员密码失败：%w", err)
	}
	password := bytes.TrimRight(line, "\r\n")
	if len(password) == 0 {
		clear(line)
		return nil, errors.New("未输入 Web 管理员密码")
	}
	return password, nil
}

func loginExposureTargets(client *http.Client, targets []string, username string, password []byte, useTLS bool) (string, error) {
	var failures []string
	for _, baseURL := range targets {
		err := loginExposure(client, baseURL, username, password)
		if err == nil {
			return baseURL, nil
		}

		var httpErr *exposureHTTPError
		if errors.As(err, &httpErr) {
			// The host answered. Trying another loopback address would hide a
			// real authentication or server error.
			return "", fmt.Errorf("Web 登录失败：%w", err)
		}
		failures = append(failures, fmt.Sprintf("%s：%v", baseURL, err))
	}
	if len(failures) == 0 {
		return "", errors.New("没有可用的 Web 控制台地址")
	}

	message := fmt.Sprintf("连接 Web 控制台失败（已尝试 %s）", strings.Join(targets, "、"))
	if useTLS {
		message += "；若 Web 控制台使用自签名证书，请显式添加 --insecure"
	}
	return "", fmt.Errorf("%s：%s", message, strings.Join(failures, "；"))
}

func loginExposure(client *http.Client, baseURL, username string, password []byte) error {
	payload := struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{
		Username: username,
		Password: string(password),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("编码登录请求失败：%w", err)
	}
	defer clear(body)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+"/api/login", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建登录请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("连接 %s 失败：%w", baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, exposureMaxBody))
	if err != nil {
		return fmt.Errorf("读取登录响应失败：%w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := decodeExposureError(responseBody)
		if resp.StatusCode == http.StatusUnauthorized {
			message = "用户名或密码错误"
		} else if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return &exposureHTTPError{
			Status:  resp.StatusCode,
			Code:    "login_failed",
			Message: message,
		}
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("解析 Web 控制台地址失败：%w", err)
	}
	if client.Jar == nil || len(client.Jar.Cookies(parsed)) == 0 {
		return errors.New("Web 登录成功但未收到会话 Cookie")
	}
	return nil
}

func applyExposure(client *http.Client, baseURL string, opts exposureOptions, stdout, stderr io.Writer) error {
	body, err := json.Marshal(struct {
		Exposure view.Exposure `json:"exposure"`
	}{Exposure: opts.mode})
	if err != nil {
		return fmt.Errorf("编码暴露范围请求失败：%w", err)
	}

	endpoint := baseURL +
		"/api/clients/" + url.PathEscape(opts.clientID) +
		"/mappings/" + url.PathEscape(opts.mappingID) +
		"/exposure"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建暴露范围请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "hy2tun")
	// RequireCSRF compares Origin with the scheme and Host used to reach the
	// console. The CLI connects directly, so the target URL is the origin.
	req.Header.Set("Origin", baseURL)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("调用暴露范围 API 失败：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, exposureMaxBody))
	if err != nil {
		return fmt.Errorf("读取暴露范围响应失败：%w", err)
	}

	if resp.StatusCode == http.StatusOK {
		var mapping view.MappingView
		if err := json.Unmarshal(responseBody, &mapping); err != nil {
			return fmt.Errorf("解析暴露范围响应失败：%w", err)
		}
		if !mapping.Exposure.Valid() {
			return fmt.Errorf("服务端返回了无效的暴露范围 %q", mapping.Exposure)
		}
		mappingID := mapping.ID
		if mappingID == "" {
			mappingID = opts.mappingID
		}
		_, _ = fmt.Fprintf(stdout, "映射 %s 的暴露范围已设置为 %s\n", mappingID, mapping.Exposure)
		if warning := formatExposureWarning(mapping.UnavailableFamilies); warning != "" {
			_, _ = fmt.Fprintf(stderr, "警告：%s\n", warning)
		}
		return nil
	}

	if resp.StatusCode == http.StatusConflict {
		var conflict exposureConflictResponse
		if err := json.Unmarshal(responseBody, &conflict); err != nil {
			return fmt.Errorf("解析暴露范围冲突响应失败：%w", err)
		}
		if conflict.Code == "exposure_conflict" {
			if conflict.Closed {
				_, _ = fmt.Fprintf(stderr, "映射 %s 已关闭，当前没有可用监听", opts.mappingID)
				if conflict.ActiveExposure.Valid() {
					_, _ = fmt.Fprintf(stderr, "（原暴露范围 %s）", conflict.ActiveExposure)
				}
				_, _ = fmt.Fprintln(stderr)
			} else {
				active := "未知"
				if conflict.ActiveExposure.Valid() {
					active = string(conflict.ActiveExposure)
				}
				_, _ = fmt.Fprintf(stderr, "映射 %s 当前仍为 %s\n", opts.mappingID, active)
			}
			if warning := formatExposureWarning(conflict.UnavailableFamilies); warning != "" {
				_, _ = fmt.Fprintf(stderr, "警告：%s\n", warning)
			}
			return errors.New("切换映射暴露范围失败")
		}
	}

	return exposureStatusError(resp.StatusCode, responseBody)
}

type exposureConflictResponse struct {
	Error               string        `json:"error"`
	Code                string        `json:"code"`
	ActiveExposure      view.Exposure `json:"active_exposure"`
	UnavailableFamilies []string      `json:"unavailable_families"`
	Closed              bool          `json:"closed"`
}

type exposureErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

type exposureHTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e *exposureHTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("HTTP %d（%s）：%s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("HTTP %d：%s", e.Status, e.Message)
}

func decodeExposureError(body []byte) string {
	var payload exposureErrorBody
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Error)
}

func exposureStatusError(status int, body []byte) error {
	var payload exposureErrorBody
	_ = json.Unmarshal(body, &payload)
	message := strings.TrimSpace(payload.Error)
	code := strings.TrimSpace(payload.Code)

	switch status {
	case http.StatusUnauthorized:
		if message == "" {
			message = "登录会话已失效，请重新运行命令"
		}
	case http.StatusForbidden:
		if message == "" {
			message = "请求被拒绝（会话或 CSRF 校验失败）"
		}
	case http.StatusNotFound:
		switch code {
		case "mapping_not_found":
			message = "映射不存在"
		case "client_not_found":
			message = "客户端不存在"
		}
	case http.StatusNotImplemented:
		if message == "" {
			message = "当前服务端不支持修改映射暴露范围"
		}
	case http.StatusInternalServerError:
		if message == "" {
			message = "服务端内部错误"
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if code != "" {
		return fmt.Errorf("切换映射暴露范围失败（HTTP %d，%s）：%s", status, code, message)
	}
	return fmt.Errorf("切换映射暴露范围失败（HTTP %d）：%s", status, message)
}

func formatExposureWarning(families []string) string {
	var (
		ipv4  bool
		ipv6  bool
		other []string
		seen  = make(map[string]bool)
	)
	for _, family := range families {
		key := strings.ToLower(strings.TrimSpace(family))
		switch key {
		case "":
			continue
		case "ipv4":
			ipv4 = true
		case "ipv6":
			ipv6 = true
		default:
			if seen[key] {
				continue
			}
			seen[key] = true
			other = append(other, strings.TrimSpace(family))
		}
	}

	labels := make([]string, 0, 2+len(other))
	if ipv4 {
		labels = append(labels, "IPv4")
	}
	if ipv6 {
		labels = append(labels, "IPv6")
	}
	labels = append(labels, other...)
	if len(labels) == 0 {
		return ""
	}
	return strings.Join(labels, "/") + " 不可用"
}
