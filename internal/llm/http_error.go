package llm

import (
	"fmt"
	"net/http"

	"github.com/samsaffron/term-llm/internal/providerhttp"
)

func newHTTPStatusError(provider string, resp *http.Response, body []byte) *providerhttp.StatusError {
	return providerhttp.NewStatusError(provider, resp, body)
}

func newHTTPStatusErrorMessageFromResponsef(resp *http.Response, format string) *providerhttp.StatusError {
	return providerhttp.NewStatusErrorMessageFromResponse(resp, func(body []byte) string {
		return fmt.Sprintf(format, resp.StatusCode, string(body))
	})
}

func newHTTPStatusErrorString(provider string, statusCode int, status string, header http.Header, body string) *providerhttp.StatusError {
	return providerhttp.NewStatusErrorString(provider, statusCode, status, header, body)
}

func newHTTPStatusErrorMessagef(resp *http.Response, body []byte, format string, args ...any) *providerhttp.StatusError {
	return providerhttp.NewStatusErrorMessage(fmt.Sprintf(format, args...), resp, body)
}

func newHTTPStatusErrorWithDisplayBody(provider string, resp *http.Response, body []byte, displayBody string) *providerhttp.StatusError {
	display := providerhttp.NewStatusErrorString(provider, resp.StatusCode, resp.Status, resp.Header, displayBody)
	return providerhttp.NewStatusErrorMessage(display.Error(), resp, body)
}
