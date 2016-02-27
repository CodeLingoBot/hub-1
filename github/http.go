package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/github/hub/ui"
	"github.com/github/hub/utils"
)

type verboseTransport struct {
	Transport   *http.Transport
	Verbose     bool
	OverrideURL *url.URL
	Out         io.Writer
	Colorized   bool
}

func (t *verboseTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	if t.Verbose {
		t.dumpRequest(req)
	}

	if t.OverrideURL != nil {
		port := "80"
		if s := strings.Split(req.URL.Host, ":"); len(s) > 1 {
			port = s[1]
		}

		req = cloneRequest(req)
		req.Header.Set("X-Original-Scheme", req.URL.Scheme)
		req.Header.Set("X-Original-Port", port)
		req.URL.Scheme = t.OverrideURL.Scheme
		req.URL.Host = t.OverrideURL.Host
	}

	resp, err = t.Transport.RoundTrip(req)

	if err == nil && t.Verbose {
		t.dumpResponse(resp)
	}

	return
}

func (t *verboseTransport) dumpRequest(req *http.Request) {
	info := fmt.Sprintf("> %s %s://%s%s", req.Method, req.URL.Scheme, req.Host, req.URL.RequestURI())
	t.verbosePrintln(info)
	t.dumpHeaders(req.Header, ">")
	body := t.dumpBody(req.Body)
	if body != nil {
		// reset body since it's been read
		req.Body = body
	}
}

func (t *verboseTransport) dumpResponse(resp *http.Response) {
	info := fmt.Sprintf("< HTTP %d", resp.StatusCode)
	t.verbosePrintln(info)
	t.dumpHeaders(resp.Header, "<")
	body := t.dumpBody(resp.Body)
	if body != nil {
		// reset body since it's been read
		resp.Body = body
	}
}

func (t *verboseTransport) dumpHeaders(header http.Header, indent string) {
	dumpHeaders := []string{"Authorization", "X-GitHub-OTP", "Location"}
	for _, h := range dumpHeaders {
		v := header.Get(h)
		if v != "" {
			r := regexp.MustCompile("(?i)^(basic|token) (.+)")
			if r.MatchString(v) {
				v = r.ReplaceAllString(v, "$1 [REDACTED]")
			}

			info := fmt.Sprintf("%s %s: %s", indent, h, v)
			t.verbosePrintln(info)
		}
	}
}

func (t *verboseTransport) dumpBody(body io.ReadCloser) io.ReadCloser {
	if body == nil {
		return nil
	}

	defer body.Close()
	buf := new(bytes.Buffer)
	_, err := io.Copy(buf, body)
	utils.Check(err)

	if buf.Len() > 0 {
		t.verbosePrintln(buf.String())
	}

	return ioutil.NopCloser(buf)
}

func (t *verboseTransport) verbosePrintln(msg string) {
	if t.Colorized {
		msg = fmt.Sprintf("\033[36m%s\033[0m", msg)
	}

	fmt.Fprintln(t.Out, msg)
}

func newHttpClient(testHost string, verbose bool) *http.Client {
	var testURL *url.URL
	if testHost != "" {
		testURL, _ = url.Parse(testHost)
	}
	tr := &verboseTransport{
		Transport: &http.Transport{
			Proxy: proxyFromEnvironment,
			Dial: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).Dial,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		Verbose:     verbose,
		OverrideURL: testURL,
		Out:         ui.Stderr,
		Colorized:   ui.IsTerminal(os.Stderr),
	}

	// Implement CheckRedirect callback to fix issues with net/http.
	fixupCheckRedirect := func(req *http.Request, via []*http.Request) error {
		// net/http doesn't send a Host header on redirect requests.
		// TODO: Find or file a Go bug.
		if req.Host == "" {
			req.Host = req.URL.Host
		}

		// Maintain headers after redirect.
		// https://github.com/golang/go/issues/4800
		for key, val := range via[0].Header {
			if req.Host != via[0].Host && strings.EqualFold(key, "Authorization") {
				continue
			}
			req.Header[key] = val
		}

		// remainder should match http/Client.defaultCheckRedirect()
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}

		return nil
	}

	return &http.Client{Transport: tr, CheckRedirect: fixupCheckRedirect}
}

func cloneRequest(req *http.Request) *http.Request {
	dup := new(http.Request)
	*dup = *req
	dup.URL, _ = url.Parse(req.URL.String())
	dup.Header = make(http.Header)
	for k, s := range req.Header {
		dup.Header[k] = s
	}
	return dup
}

// An implementation of http.ProxyFromEnvironment that isn't broken
func proxyFromEnvironment(req *http.Request) (*url.URL, error) {
	proxy := os.Getenv("http_proxy")
	if proxy == "" {
		proxy = os.Getenv("HTTP_PROXY")
	}
	if proxy == "" {
		return nil, nil
	}

	proxyURL, err := url.Parse(proxy)
	if err != nil || !strings.HasPrefix(proxyURL.Scheme, "http") {
		if proxyURL, err := url.Parse("http://" + proxy); err == nil {
			return proxyURL, nil
		}
	}

	if err != nil {
		return nil, fmt.Errorf("invalid proxy address %q: %v", proxy, err)
	}

	return proxyURL, nil
}

type simpleClient struct {
	httpClient  *http.Client
	rootUrl     *url.URL
	accessToken string
}

func (c *simpleClient) performRequest(method, path string, body io.Reader, configure func(*http.Request)) (res *simpleResponse, err error) {
	url, err := url.Parse(path)
	if err != nil {
		return
	}

	url = c.rootUrl.ResolveReference(url)
	req, err := http.NewRequest(method, url.String(), body)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "token "+c.accessToken)
	if configure != nil {
		configure(req)
	}

	httpResponse, err := c.httpClient.Do(req)
	if err == nil {
		res = &simpleResponse{httpResponse}
	}

	return
}

func (c *simpleClient) jsonRequest(method, path string, body interface{}) (*simpleResponse, error) {
	json, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(json)

	return c.performRequest(method, path, buf, func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
	})
}

func (c *simpleClient) Get(path string) (*simpleResponse, error) {
	return c.performRequest("GET", path, nil, nil)
}

func (c *simpleClient) GetFile(path string, mimeType string) (*simpleResponse, error) {
	return c.performRequest("GET", path, nil, func(req *http.Request) {
		req.Header.Set("Accept", mimeType)
	})
}

func (c *simpleClient) Delete(path string) (*simpleResponse, error) {
	return c.performRequest("DELETE", path, nil, nil)
}

func (c *simpleClient) PostJSON(path string, payload interface{}) (*simpleResponse, error) {
	return c.jsonRequest("POST", path, payload)
}

func (c *simpleClient) PatchJSON(path string, payload interface{}) (*simpleResponse, error) {
	return c.jsonRequest("PATCH", path, payload)
}

func (c *simpleClient) PostFile(path, filename string) (*simpleResponse, error) {
	stat, err := os.Stat(filename)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return c.performRequest("POST", path, file, func(req *http.Request) {
		req.ContentLength = stat.Size()
		req.Header.Set("Content-Type", "application/octet-stream")
	})
}

type simpleResponse struct {
	*http.Response
}

type errorInfo struct {
	Message  string       `json:"message"`
	Errors   []fieldError `json:"errors"`
	Response *http.Response
}
type fieldError struct {
	Resource string `json:"resource"`
	Message  string `json:"message"`
	Code     string `json:"code"`
	Field    string `json:"field"`
}

func (e *errorInfo) Error() string {
	return e.Message
}

func (res *simpleResponse) Unmarshal(dest interface{}) (err error) {
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return
	}

	return json.Unmarshal(body, dest)
}

func (res *simpleResponse) ErrorInfo() (msg *errorInfo, err error) {
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return
	}

	msg = &errorInfo{}
	err = json.Unmarshal(body, msg)
	if err == nil {
		msg.Response = res.Response
	}

	return
}