package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestCompletionProducerToActualWorkerContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node required for real worker contract")
	}
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &serveServer{store: store, cfg: serveServerConfig{uiTitle: "Synthetic fixture node"}}
	sub, err := store.UpsertPushSubscription(context.Background(), &session.PushSubscription{Endpoint: "https://example.invalid/never-sent", KeyP256DH: "key", KeyAuth: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	seedCompletionCandidate(t, s, store, "final", "session", sub.ID)
	items, err := store.ClaimCompletionNotifications(context.Background(), "test", time.Now().Add(4*time.Second), 30*time.Second, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("durable candidates=%+v err=%v", items, err)
	}
	primary, err := s.completionNotificationPayload(items[0])
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the actual compact-retry producer too, including every owner/read field.
	var compact []byte
	calls := 0
	_, _, err = sendCompletionPush(context.Background(), sub, primary, appnotify.NewWebPushOptions("key", "private", "", 300), func(_ context.Context, _ *session.PushSubscription, body []byte, _ *webpush.Options) (int, time.Duration, error) {
		calls++
		if calls == 1 {
			return 0, 0, webpush.ErrMaxPadExceeded
		}
		compact = append([]byte(nil), body...)
		return http.StatusCreated, 0, nil
	})
	if err != nil || len(compact) == 0 {
		t.Fatalf("compact payload not produced: %v", err)
	}
	for _, tc := range []struct {
		name    string
		payload []byte
	}{{"primary", primary}, {"compact", compact}} {
		t.Run(tc.name, func(t *testing.T) {
			var envelope completionPushPayload
			if err := json.Unmarshal(tc.payload, &envelope); err != nil {
				t.Fatal(err)
			}
			created, _ := time.Parse(time.RFC3339Nano, envelope.CreatedAt)
			expires, _ := time.Parse(time.RFC3339Nano, envelope.ExpiresAt)
			if expires.Sub(created) > 24*time.Hour {
				t.Fatalf("wire expiry exceeds replay horizon: %s", expires.Sub(created))
			}
			fixture := filepath.Join(t.TempDir(), "completion.json")
			if err := os.WriteFile(fixture, tc.payload, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if output, err := exec.CommandContext(ctx, node, "-e", completionWorkerContract, fixture, filepath.Join("..", "..", "serveui", "static", "sw.js")).CombinedOutput(); err != nil {
				t.Fatalf("actual durable completion producer → worker: %v\n%s", err, output)
			}
			// Opt-in interop artifacts contain only this synthetic test's actual
			// producer bytes. No repository-specific destination is embedded here.
			if dir := os.Getenv("TERM_LLM_NOTIFICATION_FIXTURE_DIR"); dir != "" {
				if !filepath.IsAbs(dir) {
					t.Fatal("fixture export directory must be absolute")
				}
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				name := "completion.json"
				if tc.name == "compact" {
					name = "completion-compact.json"
				}
				if err := os.WriteFile(filepath.Join(dir, name), tc.payload, 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

const completionWorkerContract = `
const fs=require('node:fs'),vm=require('node:vm');
const payload=JSON.parse(fs.readFileSync(process.argv[1],'utf8'));
const listeners=new Map(),stored=new Map(),shown=[];
const scope='https://example.test/ui/';
const cache={async match(k){return stored.get(String(k))?.clone()},async put(k,v){stored.set(String(k),v.clone())}};
vm.runInNewContext(fs.readFileSync(process.argv[2],'utf8'),{URL,Request,Response,console,caches:{async open(){return cache}},self:{
 location:new URL(scope),registration:{scope,async showNotification(title,options){shown.push({title,options})},async getNotifications(){return []}},
 clients:{async matchAll(){return []}},addEventListener(type,callback){listeners.set(type,callback)}
}});
(async()=>{
 for(let i=0;i<2;i++){let pending;listeners.get('push')({data:{json:()=>payload},waitUntil(p){pending=p}});await pending}
 if(shown.length!==1)throw Error('valid completion rejected/replayed: '+JSON.stringify({shown,payload}));
 const data=shown[0].options.data;
 for(const key of ['event_id','turn_id','response_id','window_id','store_instance_id','session_id','attention_seq','run_epoch','source','outcome']){
  if(data[key]!==payload[key])throw Error('lost exact identity '+key)
 }
 if(data.url!==scope+'chat/'+encodeURIComponent(payload.session_id))throw Error('wrong completion route: '+data.url);
})().catch(e=>{console.error(e);process.exitCode=1});
`
