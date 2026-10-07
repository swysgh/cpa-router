package main

import (
	"encoding/json"
)

// hostCaller is the interface through which the plugin reaches the host.
// All host interactions (host.model.*, host.log, host.stream.*) go through
// this interface so the business logic stays pure Go and unit-testable with a
// fake implementation.
type hostCaller interface {
	// Call invokes a host callback method. It returns the raw JSON result
	// (envelope "result" field) on success. On a host error envelope it returns
	// a *hostError carrying the host-reported code/message/http_status.
	Call(method string, payload any) (json.RawMessage, error)
}

// hostError is returned when a host callback returns an error envelope.
// It carries the HTTP status reported by the host so callers can branch on
// real status codes (e.g. 429) instead of string matching.
type hostError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *hostError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// StatusCode returns the host-reported HTTP status for this error.
func (e *hostError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

// asHostError extracts a *hostError from err when possible, returning nil
// otherwise.
func asHostError(err error) *hostError {
	if err == nil {
		return nil
	}
	if he, ok := err.(*hostError); ok {
		return he
	}
	return nil
}

// httpStatusFromError returns the HTTP status embedded in a hostError, or 0.
func httpStatusFromError(err error) int {
	if he := asHostError(err); he != nil {
		return he.StatusCode()
	}
	return 0
}
