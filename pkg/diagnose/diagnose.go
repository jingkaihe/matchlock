package diagnose

type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusWarn Status = "warn"
)

type Check struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

type Result struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

func finalize(checks []Check) Result {
	ok := true
	for _, check := range checks {
		if check.Status == StatusFail {
			ok = false
			break
		}
	}
	return Result{OK: ok, Checks: checks}
}

func Pass(name, message string) Check {
	return Check{Name: name, Status: StatusPass, Message: message}
}

func Warn(name, message, fix string) Check {
	return Check{Name: name, Status: StatusWarn, Message: message, Fix: fix}
}

func Fail(name, message, fix string) Check {
	return Check{Name: name, Status: StatusFail, Message: message, Fix: fix}
}
