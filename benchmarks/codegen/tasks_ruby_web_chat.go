package main

import "time"

type rubyWebChat1000Task struct{}

func (rubyWebChat1000Task) Name() string       { return "ruby_web_chat_1000" }
func (rubyWebChat1000Task) Language() string   { return "ruby" }
func (rubyWebChat1000Task) Difficulty() string { return "hard-web-concurrency" }
func (rubyWebChat1000Task) Prompt() string {
	return `Write a complete Ruby source file using only the Ruby standard library that defines exactly this method:

def new_chat_server

Return a callable object/lambda/proc. The benchmark adapter will call it as:

status, json_body = server.call(method, path, raw_json_request_body)

The third argument is a raw JSON string for POST requests; parse it with JSON.parse.

Implement a tiny in-memory multi-room chat API.

Required operations:

POST /rooms/{room}/messages
- Request body JSON: {"user":"alice","text":"hello"}
- user and text must be non-empty strings
- Append the message to that room and assign a monotonically increasing seq number starting at 1 per room
- Return [201, JSON.generate({seq: 1, user: "alice", text: "hello"})]

GET /rooms/{room}/messages
- Return [200, JSON array string] with all stored messages for that room in seq order
- Unknown rooms return []

Anything else should return an appropriate 4xx status and a JSON body.

The callable must be safe for 1000 concurrent HTTP requests posting at the same time. Do not start a server, read stdin, or print anything.`
}

func (rubyWebChat1000Task) Score(response string, timeout time.Duration) ScoreResult {
	return scoreHTTPServer(response, timeout, httpServerSpec{
		Prefix: "term-llm-codegen-bench-ruby-http",
		Files: map[string]string{
			"solution.rb": "{{CODE}}",
			"server.rb": `require 'socket'
require_relative './solution'

server_impl = new_chat_server
port = Integer(ENV.fetch('PORT'))
listener = TCPServer.new('127.0.0.1', port)

trap('TERM') { listener.close rescue nil; exit }

def read_request(socket)
  first = socket.gets("\r\n")
  return nil unless first
  method, path, = first.split(' ', 3)
  headers = {}
  while (line = socket.gets("\r\n"))
    line = line.chomp
    break if line.empty?
    key, value = line.split(':', 2)
    headers[key.downcase] = value.strip if key && value
  end
  len = headers.fetch('content-length', '0').to_i
  body = len > 0 ? socket.read(len) : nil
  [method, path, body]
end

def write_response(socket, status, body)
  body = body.to_s
  reason = {200=>'OK', 201=>'Created', 400=>'Bad Request', 404=>'Not Found', 405=>'Method Not Allowed'}[status] || 'OK'
  socket.write "HTTP/1.1 #{status} #{reason}\r\ncontent-type: application/json\r\ncontent-length: #{body.bytesize}\r\nconnection: close\r\n\r\n#{body}"
end

loop do
  begin
    sock = listener.accept
  rescue IOError
    break
  end
  Thread.new(sock) do |socket|
    begin
      req = read_request(socket)
      if req
        status, body = server_impl.call(*req)
        write_response(socket, Integer(status), body)
      end
    rescue => e
      write_response(socket, 500, '{"error":"server"}') rescue nil
    ensure
      socket.close rescue nil
    end
  end
end
`,
		},
		Cmd: []string{"ruby", "server.rb"},
	})
}
