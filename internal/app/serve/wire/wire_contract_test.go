package wire_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/app/serve/wire"
)

func steeringBody(t *testing.T, canonical bool, payload any) string {
	t.Helper()
	b, err := wire.SteeringJSON(wire.NewSteeringWriter(httptest.NewRecorder(), canonical), payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSteeringJSONProjection(t *testing.T) {
	tests := []struct {
		name      string
		canonical bool
		in        string
		want      string
	}{
		{"canonical renames and event values", true,
			`{"interjection_id":"a","interjection_status":"s","pending_interjections":[{"interjection_id":"b"}],"type":"response.interjection","event":"response.interjection","interrupt_state":"interject","action":"interject"}`,
			`{"action":"steer","event":"response.steering","interrupt_state":"steer","pending_steering":[{"steering_id":"b"}],"steering_id":"a","steering_status":"s","type":"response.steering"}`},
		{"legacy renames and event values", false,
			`{"steering_id":"a","steering_status":"s","pending_steering":[{"steering_id":"b"}],"type":"response.steering","event":"response.steering","interrupt_state":"steer","action":"steer"}`,
			`{"action":"interject","event":"response.interjection","interjection_id":"a","interjection_status":"s","interrupt_state":"interject","pending_interjections":[{"interjection_id":"b"}],"type":"response.interjection"}`},
		{"canonical drops pending text keys, including pending_steering_text", true,
			`{"pending_interjection":"t","pending_steering_text":"u","keep":1}`,
			`{"keep":1}`},
		{"legacy renames pending text and drops active_rush", false,
			`{"pending_steering_text":"t","active_rush":{"x":1},"nested":{"active_rush":true}}`,
			`{"nested":{},"pending_interjection":"t"}`},
		{"canonical keeps active_rush", true, `{"active_rush":{"x":1}}`, `{"active_rush":{"x":1}}`},
		{"opaque fields untouched canonical", true,
			`{"content":{"interjection_id":"c","type":"interject"},"parts":[{"interjection_id":"p"}],"arguments":{"interjection_id":"a"},"input":{"interjection_id":"i"},"output":{"interjection_id":"o"},"message":{"interjection_id":"m"},"text":{"interjection_id":"t"}}`,
			`{"arguments":{"interjection_id":"a"},"content":{"interjection_id":"c","type":"interject"},"input":{"interjection_id":"i"},"message":{"interjection_id":"m"},"output":{"interjection_id":"o"},"parts":[{"interjection_id":"p"}],"text":{"interjection_id":"t"}}`},
		{"opaque fields untouched legacy", false,
			`{"content":{"steering_id":"c"},"parts":[{"steering_id":"p"}],"arguments":{"steering_id":"a"},"input":{"steering_id":"i"},"output":{"steering_id":"o"},"message":{"steering_id":"m"},"text":{"steering_id":"t"}}`,
			`{"arguments":{"steering_id":"a"},"content":{"steering_id":"c"},"input":{"steering_id":"i"},"message":{"steering_id":"m"},"output":{"steering_id":"o"},"parts":[{"steering_id":"p"}],"text":{"steering_id":"t"}}`},
		{"opaque string values are not event-mapped", true,
			`{"text":"interject","content":"response.interjection"}`,
			`{"content":"response.interjection","text":"interject"}`},
		{"nested arrays and objects", true,
			`{"data":{"items":[[{"interjection_id":"x"}],{"w":[{"pending_interjections":[]}]}]}}`,
			`{"data":{"items":[[{"steering_id":"x"}],{"w":[{"pending_steering":[]}]}]}}`},
		{"non-string event values untouched", true, `{"type":1,"event":null,"action":["interject"]}`, `{"action":["interject"],"event":null,"type":1}`},
		{"other strings under type untouched", true, `{"type":"interject_other"}`, `{"type":"interject_other"}`},
		{"html is escaped", true, `{"a":"x<y>&z"}`, `{"a":"x\u003cy\u003e\u0026z"}`},
		{"top-level array", false, `[{"steering_id":"a"}]`, `[{"interjection_id":"a"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := steeringBody(t, tt.canonical, json.RawMessage(tt.in)); got != tt.want {
				t.Fatalf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestSteeringJSONPreservesLargeNumberLexemes(t *testing.T) {
	in := `{"big":12345678901234567890123456789,"frac":1.00000000000000000001,"exp":1E400,"neg0":-0,"arr":[9007199254740993]}`
	for _, canonical := range []bool{true, false} {
		if got := steeringBody(t, canonical, json.RawMessage(in)); got != `{"arr":[9007199254740993],"big":12345678901234567890123456789,"exp":1E400,"frac":1.00000000000000000001,"neg0":-0}` {
			t.Fatalf("canonical=%v got %s", canonical, got)
		}
	}
	if got := steeringBody(t, true, map[string]any{"n": json.Number("18446744073709551616")}); got != `{"n":18446744073709551616}` {
		t.Fatalf("got %s", got)
	}
}

func TestSteeringJSONMarshalError(t *testing.T) {
	if _, err := wire.SteeringJSON(httptest.NewRecorder(), map[string]any{"f": func() {}}); err == nil {
		t.Fatal("expected marshal error")
	}
}

type wrappedWriter struct{ http.ResponseWriter }

func (w wrappedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error { d.deadline = t; return nil }

func TestSteeringWriterCanonicalSelectionIsExactType(t *testing.T) {
	rec := httptest.NewRecorder()
	legacy := wire.NewSteeringWriter(rec, false)
	canon := wire.NewSteeringWriter(rec, true)
	embedded := struct{ *wire.SteeringWriter }{legacy}
	tests := []struct {
		name string
		w    io.Writer
		want bool
	}{
		{"plain recorder", rec, true},
		{"bytes buffer", &bytes.Buffer{}, true},
		{"canonical writer", canon, true},
		{"legacy writer", legacy, false},
		{"wrapper around legacy is not unwrapped", wrappedWriter{legacy}, true},
		{"embedding legacy is not exact type", embedded, true},
	}
	for _, tt := range tests {
		if got := wire.SteeringCanonical(tt.w); got != tt.want {
			t.Errorf("%s: SteeringCanonical=%v want %v", tt.name, got, tt.want)
		}
	}
	if got := steeringBody(t, false, map[string]any{"steering_id": "a"}); got != `{"interjection_id":"a"}` {
		t.Fatalf("got %s", got)
	}
}

func TestSteeringWriterUnwrapFlushAndController(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw := wire.NewSteeringWriter(rec, false)
	if sw.Unwrap() != http.ResponseWriter(rec) {
		t.Fatal("Unwrap must return the wrapped writer")
	}
	sw.Flush()
	if !rec.Flushed {
		t.Fatal("Flush must reach underlying writer")
	}
	rec.Flushed = false
	// A further wrapper reaches the SteeringWriter and then the base via Unwrap chain.
	rc := http.NewResponseController(wrappedWriter{sw})
	if err := rc.Flush(); err != nil || !rec.Flushed {
		t.Fatalf("controller flush err=%v flushed=%v", err, rec.Flushed)
	}
	want := time.Unix(1700000000, 0)
	if err := rc.SetWriteDeadline(want); err != nil || !rec.deadline.Equal(want) {
		t.Fatalf("deadline err=%v got=%v", err, rec.deadline)
	}
	// Underlying writer without Flush support: Flush is a silent no-op.
	wire.NewSteeringWriter(struct{ http.ResponseWriter }{httptest.NewRecorder()}, true).Flush()
}

func TestSSEEventBytes(t *testing.T) {
	tests := []struct {
		name      string
		canonical bool
		event     string
		payload   any
		want      string
	}{
		{"canonical spelling from legacy input", true, "response.interjection", map[string]any{"interjection_id": "a"}, "event: response.steering\ndata: {\"steering_id\":\"a\"}\n\n"},
		{"canonical spelling kept", true, "response.steering", map[string]any{"steering_id": "a"}, "event: response.steering\ndata: {\"steering_id\":\"a\"}\n\n"},
		{"legacy spelling from canonical input", false, "response.steering", map[string]any{"steering_id": "a"}, "event: response.interjection\ndata: {\"interjection_id\":\"a\"}\n\n"},
		{"legacy spelling kept", false, "response.interjection", map[string]any{"steering_id": "a"}, "event: response.interjection\ndata: {\"interjection_id\":\"a\"}\n\n"},
		{"other events untouched", false, "response.output_text.delta", map[string]any{"delta": "hi"}, "event: response.output_text.delta\ndata: {\"delta\":\"hi\"}\n\n"},
		{"empty event name", true, "", 1, "event: \ndata: 1\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if err := wire.WriteSSEEvent(wire.NewSteeringWriter(rec, tt.canonical), tt.event, tt.payload); err != nil {
				t.Fatal(err)
			}
			if got := rec.Body.String(); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
	if wire.SteeringEvent(&bytes.Buffer{}, "response.interjection") != "response.steering" {
		t.Fatal("canonical SteeringEvent")
	}
	var buf bytes.Buffer
	if err := wire.WriteChatStreamChunk(&buf, map[string]any{"id": "c"}); err != nil || buf.String() != "data: {\"id\":\"c\"}\n\n" {
		t.Fatalf("chunk %q err=%v", buf.String(), err)
	}
	buf.Reset()
	if err := wire.WriteSSEEvent(&buf, "e", func() {}); err == nil || buf.Len() != 0 {
		t.Fatalf("marshal failure must write nothing; err=%v buf=%q", err, buf.String())
	}
	rec := httptest.NewRecorder()
	wire.SetSSEHeaders(rec)
	h := rec.Header()
	if h.Get("Content-Type") != "text/event-stream" || h.Get("Cache-Control") != "no-cache" || h.Get("Connection") != "keep-alive" || h.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers %v", h)
	}
}

func gzipReq(ae string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if ae != "" {
		r.Header.Set("Accept-Encoding", ae)
	}
	return r
}

func TestWriteJSONGzipBodyThreshold(t *testing.T) {
	if wire.JSONGzipMinBytes != 512 {
		t.Fatalf("threshold %d", wire.JSONGzipMinBytes)
	}
	for _, tt := range []struct {
		n    int
		gzip bool
	}{{511, false}, {512, false}, {513, true}} {
		body := bytes.Repeat([]byte("a"), tt.n)
		rec := httptest.NewRecorder()
		wire.WriteJSONGzipBody(rec, gzipReq("gzip"), 201, body)
		res := rec.Result()
		if res.StatusCode != 201 || res.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("n=%d status/type %d %q", tt.n, res.StatusCode, res.Header.Get("Content-Type"))
		}
		if got := res.Header.Get("Content-Encoding") == "gzip"; got != tt.gzip {
			t.Fatalf("n=%d gzip=%v want %v", tt.n, got, tt.gzip)
		}
		raw := rec.Body.Bytes()
		if res.Header.Get("Content-Length") != strconv.Itoa(len(raw)) {
			t.Fatalf("n=%d content-length %q body %d", tt.n, res.Header.Get("Content-Length"), len(raw))
		}
		if tt.gzip {
			zr, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(zr)
			if err != nil {
				t.Fatal(err)
			}
			if err := zr.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, body) {
				t.Fatal("gzip round-trip mismatch")
			}
		} else if !bytes.Equal(raw, body) {
			t.Fatalf("n=%d identity body mismatch", tt.n)
		}
	}
	// Large body without gzip acceptance stays identity.
	rec := httptest.NewRecorder()
	wire.WriteJSONGzipBody(rec, gzipReq(""), 200, bytes.Repeat([]byte("a"), 2000))
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.Len() != 2000 {
		t.Fatal("identity expected without Accept-Encoding")
	}
}

func TestAddVaryCaseInsensitiveDedup(t *testing.T) {
	tests := []struct{ name, initial, want string }{
		{"empty", "", "Accept-Encoding"},
		{"other value appended", "Origin", "Origin, Accept-Encoding"},
		{"exact present", "Accept-Encoding", "Accept-Encoding"},
		{"lower case present", "accept-encoding", "accept-encoding"},
		{"upper case among list with spaces", "Origin,  ACCEPT-ENCODING ", "Origin,  ACCEPT-ENCODING "},
		{"prefix is not a match", "Accept-Encoding-X", "Accept-Encoding-X, Accept-Encoding"},
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.initial != "" {
			h.Set("Vary", tt.initial)
		}
		wire.AddVary(h, "Accept-Encoding")
		wire.AddVary(h, "accept-encoding")
		if got := h.Get("Vary"); got != tt.want {
			t.Errorf("%s: got %q want %q", tt.name, got, tt.want)
		}
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Vary", "ACCEPT-ENCODING")
	wire.WriteJSONGzipBody(rec, gzipReq(""), 200, []byte("{}"))
	if got := rec.Header().Values("Vary"); len(got) != 1 || got[0] != "ACCEPT-ENCODING" {
		t.Fatalf("Vary %q", got)
	}
}

func TestAcceptsGzipNegotiation(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", false}, {"gzip", true}, {"GZip", true}, {" gzip ", true},
		{"deflate, gzip", true}, {"identity", false}, {"*", false}, {"x-gzip", false},
		{"gzip;q=0", false}, {"gzip;q=0.0", false}, {"gzip; q=0", false}, {"gzip;Q=0", false},
		{"gzip;q=-1", false}, {"gzip;q=0.001", true}, {"gzip;q=1", true},
		{"gzip;q=abc", true}, {"gzip;q=", true}, {"gzip;q=NaN", true},
		{"gzip;level=1", true}, {"gzip;level=1;q=0", false},
		{"gzip;q=0, gzip", false}, {"gzip, gzip;q=0", true}, {"deflate, gzip;q=0", false},
	}
	for _, tt := range tests {
		if got := wire.AcceptsGzip(tt.in); got != tt.want {
			t.Errorf("AcceptsGzip(%q)=%v want %v", tt.in, got, tt.want)
		}
	}
}

func TestWriteJSONConditionalETag(t *testing.T) {
	small := map[string]any{"steering_id": "a"}
	body := `{"steering_id":"a"}`
	etag := wire.JSONPayloadETag([]byte(body))
	if len(etag) != 66 || etag[0] != '"' || etag[65] != '"' {
		t.Fatalf("etag shape %q", etag)
	}
	do := func(inm string) *httptest.ResponseRecorder {
		r := gzipReq("gzip")
		if inm != "" {
			r.Header.Set("If-None-Match", inm)
		}
		rec := httptest.NewRecorder()
		wire.WriteJSONConditional(rec, r, 200, small)
		return rec
	}
	for _, inm := range []string{etag, "W/" + etag, "*", `"x", ` + etag, `"x",W/` + etag} {
		rec := do(inm)
		if rec.Code != 304 || rec.Body.Len() != 0 {
			t.Fatalf("inm=%q code=%d body=%q", inm, rec.Code, rec.Body.String())
		}
		h := rec.Header()
		if h.Get("ETag") != etag || h.Get("Cache-Control") != "no-cache" || h.Get("Content-Type") != "application/json" || h.Get("Vary") != "Accept-Encoding" || h.Get("Content-Length") != "" {
			t.Fatalf("304 headers %v", h)
		}
	}
	for _, inm := range []string{"", `"nope"`, etag[:len(etag)-1], "w/" + etag} {
		rec := do(inm)
		if rec.Code != 200 || rec.Body.String() != body || rec.Header().Get("ETag") != etag || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("inm=%q code=%d body=%q hdr=%v", inm, rec.Code, rec.Body.String(), rec.Header())
		}
	}
	if !wire.ETagMatches("*", "x") || wire.ETagMatches("", "x") || wire.ETagMatches("*", "") {
		t.Fatal("ETagMatches edge cases")
	}
	// ETag hashes the uncompressed projected body even when the response is gzipped.
	big := map[string]any{"interjection_id": strings.Repeat("z", 600)}
	bigBody := `{"steering_id":"` + strings.Repeat("z", 600) + `"}`
	rec := httptest.NewRecorder()
	wire.WriteJSONConditional(rec, gzipReq("gzip"), 200, big)
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("ETag") != wire.JSONPayloadETag([]byte(bigBody)) {
		t.Fatalf("gzip etag headers %v", rec.Header())
	}
}

func TestCompressibleContentType(t *testing.T) {
	for in, want := range map[string]bool{
		"text/html; charset=utf-8": true, "TEXT/plain": true, "application/json": true, " Application/JSON ;x=1": true,
		"application/javascript": true, "image/svg+xml": true, "application/ld+json": true, "application/atom+xml": true,
		"image/png": false, "application/octet-stream": false, "": false,
	} {
		if got := wire.CompressibleContentType(in); got != want {
			t.Errorf("%q: got %v want %v", in, got, want)
		}
	}
}
