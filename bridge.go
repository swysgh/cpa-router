package main

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// hostCallerImpl is the real hostCaller. It delegates the C ABI host call to
// cgoCallHost (defined in abi.go, the only other file that imports "C") and
// parses the envelope here in pure Go. On a host error envelope it returns a
// *hostError carrying the host-reported http_status (§9).
type hostCallerImpl struct{}

func newHostCaller() *hostCallerImpl { return &hostCallerImpl{} }

// Call marshals payload to JSON, invokes the host callback through the C ABI,
// and parses the envelope. On a host error envelope it returns a *hostError.
func (h *hostCallerImpl) Call(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, errMarshal)
	}

	rawResponse, callCode, callErr := cgoCallHost(method, rawPayload)
	if callErr != nil {
		return nil, callErr
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s 返回空响应, code=%d", method, callCode)
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("解析宿主信封 %s 失败: %w", method, errUnmarshal)
	}
	if !env.OK {
		he := &hostError{Code: "", Message: "host callback " + method + " failed", HTTPStatus: 0}
		if env.Error != nil {
			he.Code = env.Error.Code
			he.Message = env.Error.Message
			he.HTTPStatus = env.Error.HTTPStatus
		}
		return nil, he
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s 返回 code=%d", method, callCode)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

// ensure pluginabi import is referenced (used indirectly via method constants).
var _ = pluginabi.MethodHostModelExecute
