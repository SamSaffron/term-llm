package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestBuildPromptRequiresSelfValidation(t *testing.T) {
	prompt := buildPrompt(fizzBuzzTask{})
	checks := []string{
		"Self-validation requirement before final output",
		"required function/type/export name",
		"includes every required import/header",
		"If you find a problem, fix it before returning the code block",
	}
	for _, check := range checks {
		if !strings.Contains(prompt, check) {
			t.Fatalf("prompt missing %q:\n%s", check, prompt)
		}
	}
}

func TestExtractCodeFencedGoBlock(t *testing.T) {
	code, err := extractCode("prose\n```go\npackage main\nfunc X() {}\n```\nmore")
	if err != nil {
		t.Fatalf("extractCode failed: %v", err)
	}
	if !strings.Contains(code, "func X") {
		t.Fatalf("unexpected code: %q", code)
	}
}

func TestExtractCodeFencedJSBlock(t *testing.T) {
	code, err := extractCode("```javascript\nexport function newChatServer() {}\n```")
	if err != nil {
		t.Fatalf("extractCode failed: %v", err)
	}
	if !strings.Contains(code, "newChatServer") {
		t.Fatalf("unexpected code: %q", code)
	}
}

func TestScoreGoFunctionPasses(t *testing.T) {
	result := scoreGoFunction(`package main

func BinarySearch(xs []int, target int) int {
	lo, hi := 0, len(xs)-1
	for lo <= hi {
		mid := lo + (hi-lo)/2
		if xs[mid] == target { return mid }
		if xs[mid] < target { lo = mid + 1 } else { hi = mid - 1 }
	}
	return -1
}`, 10*time.Second, `
func TestGenerated(t *testing.T) {
	if got := BinarySearch([]int{1, 3, 5}, 3); got != 1 {
		t.Fatalf("got %d", got)
	}
}
`)
	if !result.Pass || result.Score != 1 {
		t.Fatalf("expected pass, got %#v", result)
	}
}

func TestWebChat1000TaskReferenceImplementationPasses(t *testing.T) {
	code := `package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

type storedMessage struct {
	Seq  int    ` + "`" + `json:"seq"` + "`" + `
	User string ` + "`" + `json:"user"` + "`" + `
	Text string ` + "`" + `json:"text"` + "`" + `
}

type chatServer struct {
	mu    sync.Mutex
	rooms map[string][]storedMessage
}

func NewChatServer() http.Handler {
	return &chatServer{rooms: make(map[string][]storedMessage)}
}

func (s *chatServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "rooms" || parts[2] != "messages" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	room := parts[1]
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodPost:
		var in struct { User, Text string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.User == "" || in.Text == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		msg := storedMessage{Seq: len(s.rooms[room]) + 1, User: in.User, Text: in.Text}
		s.rooms[room] = append(s.rooms[room], msg)
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(msg)
	case http.MethodGet:
		s.mu.Lock()
		messages := append([]storedMessage(nil), s.rooms[room]...)
		s.mu.Unlock()
		if messages == nil { messages = []storedMessage{} }
		_ = json.NewEncoder(w).Encode(messages)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
`
	result := webChat1000Task{}.Score(code, 60*time.Second)
	if !result.Pass || result.Score != 1 || result.Metrics.RuntimeMS <= 0 {
		t.Fatalf("expected pass with runtime metric, detail=%s stdout=%s stderr=%s", result.Details, result.Stdout, result.Stderr)
	}
}

func TestNodeWebChat1000TaskReferenceImplementationPasses(t *testing.T) {
	code := `
import { URL } from 'node:url';

const rooms = new Map();

function readBody(req) {
  return new Promise((resolve, reject) => {
    let data = '';
    req.setEncoding('utf8');
    req.on('data', chunk => { data += chunk; });
    req.on('end', () => resolve(data));
    req.on('error', reject);
  });
}

function send(res, status, value) {
  res.writeHead(status, { 'content-type': 'application/json' });
  res.end(JSON.stringify(value));
}

export function newChatServer() {
  return async function handler(req, res) {
    const url = new URL(req.url, 'http://localhost');
    const parts = url.pathname.split('/').filter(Boolean);
    if (parts.length !== 3 || parts[0] !== 'rooms' || parts[2] !== 'messages' || !parts[1]) {
      res.writeHead(404).end();
      return;
    }
    const room = parts[1];
    if (req.method === 'POST') {
      let body;
      try { body = JSON.parse(await readBody(req)); } catch { res.writeHead(400).end(); return; }
      if (!body || typeof body.user !== 'string' || body.user === '' || typeof body.text !== 'string' || body.text === '') {
        res.writeHead(400).end();
        return;
      }
      const messages = rooms.get(room) || [];
      const msg = { seq: messages.length + 1, user: body.user, text: body.text };
      messages.push(msg);
      rooms.set(room, messages);
      send(res, 201, msg);
      return;
    }
    if (req.method === 'GET') {
      send(res, 200, rooms.get(room) || []);
      return;
    }
    res.writeHead(405).end();
  }
}
`
	result := nodeWebChat1000Task{}.Score(code, 60*time.Second)
	if !result.Pass || result.Score != 1 || result.Metrics.RuntimeMS <= 0 {
		t.Fatalf("expected pass with runtime metric, detail=%s stdout=%s stderr=%s", result.Details, result.Stdout, result.Stderr)
	}
}

func TestRubyWebChat1000TaskReferenceImplementationPasses(t *testing.T) {
	code := `
require 'json'
require 'thread'

def new_chat_server
  mutex = Mutex.new
  rooms = Hash.new { |h, k| h[k] = [] }
  lambda do |method, path, body|
    parts = path.split('/').reject(&:empty?)
    return [404, '{}'] unless parts.length == 3 && parts[0] == 'rooms' && parts[2] == 'messages' && !parts[1].empty?
    room = parts[1]
    case method
    when 'POST'
      data = JSON.parse(body)
      return [400, '{}'] unless data['user'].is_a?(String) && data['text'].is_a?(String) && data['user'] != '' && data['text'] != ''
      msg = nil
      mutex.synchronize do
        msg = {'seq' => rooms[room].length + 1, 'user' => data['user'], 'text' => data['text']}
        rooms[room] << msg
      end
      [201, JSON.generate(msg)]
    when 'GET'
      messages = mutex.synchronize { rooms[room].map(&:dup) }
      [200, JSON.generate(messages)]
    else
      [405, '{}']
    end
  end
end
`
	result := rubyWebChat1000Task{}.Score(code, 60*time.Second)
	if !result.Pass || result.Score != 1 || result.Metrics.RuntimeMS <= 0 || result.Metrics.WarmupMS <= 0 || result.Metrics.MemoryKB <= 0 {
		t.Fatalf("expected pass with runtime/warmup/memory, detail=%s stdout=%s stderr=%s", result.Details, result.Stdout, result.Stderr)
	}
}

func TestPythonWebChat1000TaskReferenceImplementationPasses(t *testing.T) {
	code := `
import json
import threading

class ChatServer:
    def __init__(self):
        self.lock = threading.Lock()
        self.rooms = {}
    def __call__(self, method, path, body):
        parts = [p for p in path.split('/') if p]
        if len(parts) != 3 or parts[0] != 'rooms' or parts[2] != 'messages' or not parts[1]:
            return 404, '{}'
        room = parts[1]
        if method == 'POST':
            try:
                data = json.loads(body)
            except Exception:
                return 400, '{}'
            if not isinstance(data.get('user'), str) or not data['user'] or not isinstance(data.get('text'), str) or not data['text']:
                return 400, '{}'
            with self.lock:
                messages = self.rooms.setdefault(room, [])
                msg = {'seq': len(messages) + 1, 'user': data['user'], 'text': data['text']}
                messages.append(msg)
            return 201, json.dumps(msg)
        if method == 'GET':
            with self.lock:
                messages = list(self.rooms.get(room, []))
            return 200, json.dumps(messages)
        return 405, '{}'

def new_chat_server():
    return ChatServer()
`
	result := pythonWebChat1000Task{}.Score(code, 60*time.Second)
	if !result.Pass || result.Score != 1 || result.Metrics.RuntimeMS <= 0 || result.Metrics.WarmupMS <= 0 || result.Metrics.MemoryKB <= 0 {
		t.Fatalf("expected pass with runtime/warmup/memory, detail=%s stdout=%s stderr=%s", result.Details, result.Stdout, result.Stderr)
	}
}

func TestAssemblySumPositiveTaskReferenceImplementationPasses(t *testing.T) {
	code := `
.text
.globl sum_positive
.type sum_positive, @function
sum_positive:
    xor %rax, %rax
    test %rsi, %rsi
    jle .done
.loop:
    mov (%rdi), %rdx
    test %rdx, %rdx
    jle .skip
    add %rdx, %rax
.skip:
    add $8, %rdi
    dec %rsi
    jne .loop
.done:
    ret
`
	result := assemblySumPositiveTask{}.Score(code, 60*time.Second)
	if !result.Pass || result.Score != 1 || result.Metrics.RuntimeMS <= 0 || result.Metrics.WarmupMS <= 0 || result.Metrics.MemoryKB <= 0 {
		t.Fatalf("expected pass with runtime/warmup/memory, detail=%s stdout=%s stderr=%s", result.Details, result.Stdout, result.Stderr)
	}
}

func TestZigSumPositiveTaskReferenceImplementationPasses(t *testing.T) {
	if _, err := exec.LookPath("zig"); err != nil {
		t.Skip("zig not installed")
	}
	code := `
export fn sum_positive(xs: [*]const i64, n: isize) i64 {
    if (n <= 0) return 0;
    var sum: i64 = 0;
    var i: isize = 0;
    while (i < n) : (i += 1) {
        const v = xs[@intCast(i)];
        if (v > 0) sum += v;
    }
    return sum;
}
`
	result := zigSumPositiveTask{}.Score(code, 60*time.Second)
	if !result.Pass || result.Score != 1 || result.Metrics.RuntimeMS <= 0 || result.Metrics.WarmupMS <= 0 || result.Metrics.MemoryKB <= 0 {
		t.Fatalf("expected pass with runtime/warmup/memory, detail=%s stdout=%s stderr=%s", result.Details, result.Stdout, result.Stderr)
	}
}
