package main

import "time"

type pythonWebChat1000Task struct{}

func (pythonWebChat1000Task) Name() string       { return "python_web_chat_1000" }
func (pythonWebChat1000Task) Language() string   { return "python" }
func (pythonWebChat1000Task) Difficulty() string { return "hard-web-concurrency" }
func (pythonWebChat1000Task) Prompt() string {
	return `Write a complete Python 3 source file using only the Python standard library that defines exactly this function:

def new_chat_server():

Return a callable object/function. The benchmark adapter will call it as:

status, json_body = server(method, path, raw_json_request_body)

The third argument is a raw JSON string for POST requests; parse it with json.loads.

Implement a tiny in-memory multi-room chat API.

Required operations:

POST /rooms/{room}/messages
- Request body JSON: {"user":"alice","text":"hello"}
- user and text must be non-empty strings
- Append the message to that room and assign a monotonically increasing seq number starting at 1 per room
- Return (201, JSON string like {"seq":1,"user":"alice","text":"hello"})

GET /rooms/{room}/messages
- Return (200, JSON array string) with all stored messages for that room in seq order
- Unknown rooms return []

Anything else should return an appropriate 4xx status and a JSON body.

The callable must be safe for 1000 concurrent HTTP requests posting at the same time. Do not start a server, read stdin, or print anything.`
}

func (pythonWebChat1000Task) Score(response string, timeout time.Duration) ScoreResult {
	return scoreHTTPServer(response, timeout, httpServerSpec{
		Prefix: "term-llm-codegen-bench-python-http",
		Files: map[string]string{
			"solution.py": "{{CODE}}",
			"server.py": `import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from solution import new_chat_server

app = new_chat_server()

class Handler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def log_message(self, fmt, *args):
        pass
    def _dispatch(self):
        length = int(self.headers.get('content-length', '0'))
        body = self.rfile.read(length).decode('utf-8') if length else None
        try:
            status, response = app(self.command, self.path, body)
            response = str(response).encode('utf-8')
            self.send_response(int(status))
            self.send_header('content-type', 'application/json')
            self.send_header('content-length', str(len(response)))
            self.send_header('connection', 'close')
            self.end_headers()
            self.wfile.write(response)
        except Exception:
            response = b'{"error":"server"}'
            self.send_response(500)
            self.send_header('content-type', 'application/json')
            self.send_header('content-length', str(len(response)))
            self.send_header('connection', 'close')
            self.end_headers()
            self.wfile.write(response)
    def do_GET(self):
        self._dispatch()
    def do_POST(self):
        self._dispatch()

class Server(ThreadingHTTPServer):
    request_queue_size = 2048

port = int(os.environ['PORT'])
Server(('127.0.0.1', port), Handler).serve_forever()
`,
		},
		Cmd: []string{"python3", "server.py"},
	})
}
