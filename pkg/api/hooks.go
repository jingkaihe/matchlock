package api

import "net/http"

type HTTPHooks struct {
	OnRequest  func(req *http.Request) (*http.Request, error)
	OnResponse func(resp *http.Response, req *http.Request) (*http.Response, error)
}
