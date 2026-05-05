package main

import "time"

type webChat1000Task struct{}

func (webChat1000Task) Name() string       { return "go_web_chat_1000" }
func (webChat1000Task) Language() string   { return "go" }
func (webChat1000Task) Difficulty() string { return "hard-web-concurrency" }
func (webChat1000Task) Prompt() string {
	return `Write a complete Go source file for package main, including any imports, that defines exactly this function:

func NewChatServer() http.Handler

Implement a tiny in-memory multi-room web chat API using only the Go standard library.

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

The handler must be safe for 1000 concurrent users posting at the same time. Do not include a main function.`
}

func (webChat1000Task) Score(response string, timeout time.Duration) ScoreResult {
	return scoreHTTPServer(response, timeout, httpServerSpec{
		Prefix: "term-llm-codegen-bench-go-http",
		Files: map[string]string{
			"solution.go": "{{CODE}}",
			"go.mod":      "module benchsolution\n\ngo 1.22\n",
			"main.go": `package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, NewChatServer()))
}
`,
		},
		Cmd: []string{"go", "run", "."},
	})
}
