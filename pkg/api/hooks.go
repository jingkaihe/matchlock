package api

import "net/http"

type HTTPHooks struct {
	OnRequest  func(req *http.Request) (*http.Request, error)
	OnResponse func(resp *http.Response, req *http.Request) (*http.Response, error)
}

type VFSHooks struct {
	BeforeOpen  func(path string, flags int) error
	AfterRead   func(path string, n int)
	AfterWrite  func(path string, n int)
	BeforeClose func(path string)
}
