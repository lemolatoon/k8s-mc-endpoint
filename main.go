package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	namespace     = "minecraft-skies2"
	labelSelector = "app=minecraft-skies2"
	addr          = ":8080"
)

var (
	scheme   = runtime.NewScheme()
	upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
)

func main() {
	v1.AddToScheme(scheme)

	cfg, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "in‑cluster config error:", err)
		os.Exit(1)
	}
	cli, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "k8s client error:", err)
		os.Exit(1)
	}

	http.HandleFunc("/logs", logsHandler(cli))
	http.HandleFunc("/list", listHandler(cli, cfg))
	http.HandleFunc("/chats", chatsHandler(cli, cfg))
	http.HandleFunc("/kill", killHandler(cli))

	fmt.Println("Server listening on", addr)
	_ = http.ListenAndServe(addr, nil)
}

// killHandler deletes all Pods matching the label selector in the target namespace
func killHandler(cli *kubernetes.Clientset) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost && r.Method != http.MethodDelete {
            w.Header().Set("Allow", "POST, DELETE")
            http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
            return
        }
        ctx := r.Context()
        pods, err := cli.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        if len(pods.Items) == 0 {
            w.Header().Set("Content-Type", "text/plain; charset=utf-8")
            w.WriteHeader(http.StatusOK)
            _, _ = w.Write([]byte("no pods to delete\n"))
            return
        }
        var (
            failed []string
            deleted []string
        )
        gp := int64(0)
        opts := metav1.DeleteOptions{GracePeriodSeconds: &gp}
        for _, p := range pods.Items {
            if err := cli.CoreV1().Pods(namespace).Delete(ctx, p.Name, opts); err != nil {
                failed = append(failed, fmt.Sprintf("%s (%v)", p.Name, err))
            } else {
                deleted = append(deleted, p.Name)
            }
        }
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        if len(failed) > 0 {
            w.WriteHeader(http.StatusMultiStatus)
        } else {
            w.WriteHeader(http.StatusOK)
        }
        if len(deleted) > 0 {
            _, _ = w.Write([]byte("deleted: " + strings.Join(deleted, ", ") + "\n"))
        }
        if len(failed) > 0 {
            _, _ = w.Write([]byte("failed: " + strings.Join(failed, "; ") + "\n"))
        }
    }
}

// logsHandler streams logs of all matching Pods
func logsHandler(cli *kubernetes.Clientset) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		pods, err := cli.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var buf bytes.Buffer
		for _, p := range pods.Items {
			req := cli.CoreV1().Pods(namespace).GetLogs(p.Name, &v1.PodLogOptions{})
			s, err := req.Stream(context.Background())
			if err != nil {
				continue
			}
			io.Copy(&buf, s)
			s.Close()
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(buf.Bytes())
	}
}

// listHandler sends "/list" to the server, waits 3 s, returns matching line(s)
func listHandler(cli *kubernetes.Clientset, cfg *rest.Config) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
		pods, err := cli.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector, Limit: 1})
		if err != nil || len(pods.Items) == 0 {
			http.Error(w, "no target pod", http.StatusInternalServerError)
			return
		}
		pod := pods.Items[0]

		var out bytes.Buffer
		pr, pw := io.Pipe()

		attach := cli.CoreV1().RESTClient().Post().Resource("pods").Name(pod.Name).
			Namespace(namespace).SubResource("attach").
			VersionedParams(&v1.PodAttachOptions{
				Container: pod.Spec.Containers[0].Name,
				Stdin:     true,
				Stdout:    true,
				Stderr:    true,
				TTY:       true,
			}, runtime.NewParameterCodec(scheme))

        exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", attach.URL())
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }

        ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
        defer cancel()
        go exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: pr, Stdout: &out, Stderr: &out, Tty: true})
        pw.Write([]byte("/list\n"))
        time.Sleep(100 * time.Millisecond)
        pw.Close()

		// extract lines like: "There are X of a max ... players online: ..."
		re := regexp.MustCompile(`There are .* players online:`)
		var matches []string
		for _, line := range strings.Split(out.String(), "\n") {
			if re.MatchString(line) {
				matches = append(matches, strings.TrimSpace(line))
			}
		}
		result := strings.Join(matches, "\n")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if result == "" {
			result = "(no player list received)"
			w.Write([]byte(result + "\n"))
			return
		}
		tsRe := regexp.MustCompile(`\[[0-9]{2}:[0-9]{2}:[0-9]{2}\]`)
		for _, raw := range strings.Split(result, "\n") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			// keep substring starting from timestamp, if present
			idx := tsRe.FindStringIndex(raw)
			if idx != nil {
				raw = raw[idx[0]:] // drop anything to the left of timestamp
			} else if raw == "" || raw == "\n" {
				continue
			}
			w.Write([]byte(raw + "\n"))
		}
	}
}

// chatsHandler proxies WebSocket <‑> pod TTY
func chatsHandler(cli *kubernetes.Clientset, cfg *rest.Config) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        conn, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
            return
        }
        defer conn.Close()

		// --- resilient attach & reconnect every 5s until WS closes ---
        type streamHandle struct {
            pw     *io.PipeWriter
            done   chan struct{}
            cancel context.CancelFunc
        }

		var (
			cur *streamHandle
		)

        attachToFirstPod := func(ctx context.Context) (*streamHandle, error) {
            pods, err := cli.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector, Limit: 1})
            if err != nil || len(pods.Items) == 0 {
                return nil, fmt.Errorf("no pod")
            }
            pod := pods.Items[0]
            pr, pw := io.Pipe()
            attach := cli.CoreV1().RESTClient().Post().Resource("pods").Name(pod.Name).
                Namespace(namespace).SubResource("attach").
                VersionedParams(&v1.PodAttachOptions{
                    Container: pod.Spec.Containers[0].Name,
                    Stdin:     true,
                    Stdout:    true,
                    Stderr:    true,
                    TTY:       true,
                }, runtime.NewParameterCodec(scheme))
            ex, err := remotecommand.NewSPDYExecutor(cfg, "POST", attach.URL())
            if err != nil {
                return nil, err
            }
            done := make(chan struct{})
            sctx, cancel := context.WithCancel(ctx)
            go func() {
                _ = ex.StreamWithContext(sctx, remotecommand.StreamOptions{Stdin: pr, Stdout: &wsWriter{conn}, Stderr: &wsWriter{conn}, Tty: true})
                close(done)
            }()
            return &streamHandle{pw: pw, done: done, cancel: cancel}, nil
        }

		// goroutine: read from websocket and forward to current stream if available
		msgCh := make(chan []byte)
		go func() {
			defer close(msgCh)
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				// append newline if not present to mimic terminal enter
				msgCh <- msg
			}
		}()

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		// main loop: maintain connection and forward messages
        baseCtx := r.Context()
        for {
            // ensure we have an active stream
            if cur == nil {
                if h, err := attachToFirstPod(baseCtx); err == nil {
                    cur = h
                    _ = conn.WriteMessage(websocket.TextMessage, []byte("attached to pod"))
                } else {
                    // inform client once per attempt tick via ticker
                }
            }

			select {
			case msg, ok := <-msgCh:
                if !ok { // websocket closed
                    if cur != nil {
                        cur.pw.Close()
                        cur.cancel()
                    }
                    return
                }
                if cur != nil {
                    _, _ = cur.pw.Write(msg)
                } else {
                    // optionally notify user that input is dropped
                }
            case <-ticker.C:
                // if not connected, try again
                if cur == nil {
                    if h, err := attachToFirstPod(baseCtx); err == nil {
                        cur = h
                        _ = conn.WriteMessage(websocket.TextMessage, []byte("attached to pod"))
                    } else {
                        _ = conn.WriteMessage(websocket.TextMessage, []byte("waiting for pod..."))
                    }
                }
            case <-func() <-chan struct{} {
                if cur == nil {
                    ch := make(chan struct{})
                    close(ch)
                    return ch
                }
                return cur.done
            }():
                // stream ended (likely pod deleted); clear and retry on next tick
                if cur != nil {
                    cur.pw.Close()
                    cur.cancel()
                }
                cur = nil
                _ = conn.WriteMessage(websocket.TextMessage, []byte("pod disconnected; retrying..."))
            }
        }
    }
}

type wsWriter struct{ conn *websocket.Conn }

func (w *wsWriter) Write(p []byte) (int, error) {
	cleaned := string(p)

	tsRe := regexp.MustCompile(`\[[0-9]{2}:[0-9]{2}:[0-9]{2}\]`)

	for _, raw := range strings.Split(cleaned, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// keep substring starting from timestamp, if present
		idx := tsRe.FindStringIndex(raw)
		if idx != nil {
			raw = raw[idx[0]:] // drop anything to the left of timestamp
		} else {
			continue
		}
		if raw == "" {
			continue
		}
		if raw == "\n" {
			continue
		}
		if err := w.conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
