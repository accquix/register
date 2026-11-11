package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// Problem is an RFC 9457 problem document with a stable machine-readable code.
type Problem struct {
	Type   string              `json:"type,omitempty" doc:"A URI reference for the problem type"`
	Title  string              `json:"title" doc:"Short summary of the problem type"`
	Status int                 `json:"status" doc:"HTTP status code"`
	Detail string              `json:"detail,omitempty" doc:"Explanation specific to this occurrence"`
	Code   string              `json:"code" doc:"Stable error code: invalid_request, invalid_identifier, unauthorized, forbidden, not_found, rate_limited, fuzzy_disabled, unavailable, internal"`
	Reason string              `json:"reason,omitempty" doc:"For unavailable: bootstrapping, setup, source_stale, timeout, database, overloaded"`
	Errors []*huma.ErrorDetail `json:"errors,omitempty" doc:"Validation details"`
}

func (p *Problem) Error() string { return p.Detail }

// GetStatus implements huma.StatusError.
func (p *Problem) GetStatus() int { return p.Status }

// ContentType implements huma.ContentTypeFilter.
func (p *Problem) ContentType(string) string { return "application/problem+json" }

var defaultCodes = map[int]string{
	400: "invalid_request", 401: "unauthorized", 403: "forbidden", 404: "not_found", 422: "invalid_request",
	429: "rate_limited", 500: "internal", 503: "unavailable",
}

func problem(status int, code, detail string) *Problem {
	return &Problem{Title: http.StatusText(status), Status: status, Code: code, Detail: detail}
}

func unavailable(reason, detail string) *Problem {
	p := problem(http.StatusServiceUnavailable, "unavailable", detail)
	p.Reason = reason
	return p
}

func init() {
	// every error Huma produces (validation, unknown routes inside the API) becomes a Problem
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		p := problem(status, defaultCodes[status], msg)
		if p.Code == "" {
			p.Code = "error"
		}
		for _, e := range errs {
			if e == nil {
				continue
			}
			if d, ok := e.(huma.ErrorDetailer); ok {
				p.Errors = append(p.Errors, d.ErrorDetail())
			} else {
				p.Errors = append(p.Errors, &huma.ErrorDetail{Message: e.Error()})
			}
		}
		return p
	}
}

// writeProblem writes a problem outside Huma (middleware).
func writeProblem(w http.ResponseWriter, p *Problem, retryAfter time.Duration) {
	w.Header().Set("Content-Type", "application/problem+json")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds()+0.999)))
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

func withRetryAfter(p *Problem, d time.Duration) error {
	return huma.ErrorWithHeaders(p, http.Header{"Retry-After": {strconv.Itoa(int(d.Seconds() + 0.999))}})
}
