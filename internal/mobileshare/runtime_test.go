package mobileshare

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRuntimeOffAndUnavailableMakeNoOutboundCalls(t *testing.T) {
	var calls atomic.Int64
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer broker.Close()
	for _, authenticated := range []bool{true, false} {
		runtime, err := New(Options{DataDir: t.TempDir(), Target: "agent", StoreInstanceID: "store", Authenticated: authenticated, BrokerURL: broker.URL, Handler: http.NotFoundHandler()})
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Start(); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("off/unavailable runtime made %d outbound calls", got)
	}
}

func TestOwnerAPIRejectsDeviceAndCrossSiteMutation(t *testing.T) {
	runtime, err := New(Options{DataDir: t.TempDir(), Target: "agent", StoreInstanceID: "store", Authenticated: false})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	request := httptest.NewRequest(http.MethodGet, "/v1/mobile-sharing", nil)
	request = WithDevicePrincipal(request, DevicePrincipal{DeviceID: "device", Scope: ScopeWebV1})
	recorder := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("device owner request = %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/mobile-sharing/stop", nil)
	request.Header.Set("Origin", "https://attacker.invalid")
	recorder = httptest.NewRecorder()
	runtime.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-site owner mutation = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}
