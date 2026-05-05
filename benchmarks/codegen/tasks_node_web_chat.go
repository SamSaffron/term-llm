package main

import "time"

type nodeWebChat1000Task struct{}

func (nodeWebChat1000Task) Name() string       { return "node_web_chat_1000" }
func (nodeWebChat1000Task) Language() string   { return "javascript" }
func (nodeWebChat1000Task) Difficulty() string { return "hard-web-concurrency" }
func (nodeWebChat1000Task) Prompt() string {
	return `Write a complete Node.js ES module using only the Node standard library that exports exactly this function:

export function newChatServer()

Return an HTTP request listener suitable for http.createServer(newChatServer()).

Implement a tiny in-memory multi-room web chat API.

Required endpoints:

POST /rooms/{room}/messages
- Request body JSON: {"user":"alice","text":"hello"}
- user and text must be non-empty strings
- Append the message to that room and assign a monotonically increasing seq number starting at 1 per room
- Return HTTP 201 with the stored message as JSON: {"seq":1,"user":"alice","text":"hello"}

GET /rooms/{room}/messages
- Return HTTP 200 with a JSON array of all stored messages for that room in seq order
- Unknown rooms return an empty JSON array []

Anything else should return an appropriate 4xx status.

The handler must survive 1000 concurrent users posting at the same time. Do not start a server or read stdin.`
}

func (nodeWebChat1000Task) Score(response string, timeout time.Duration) ScoreResult {
	return scoreHTTPServer(response, timeout, httpServerSpec{
		Prefix: "term-llm-codegen-bench-node-http",
		Files: map[string]string{
			"solution.mjs": "{{CODE}}",
			"server.mjs": `import http from 'node:http';
import { newChatServer } from './solution.mjs';

const port = Number(process.env.PORT || 8080);
const server = http.createServer(newChatServer());
server.listen(port, '127.0.0.1');
`,
		},
		Cmd: []string{"node", "server.mjs"},
	})
}
