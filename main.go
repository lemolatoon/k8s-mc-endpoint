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

	fmt.Println("Server listening on", addr)
	_ = http.ListenAndServe(addr, nil)
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
	return func(w http.ResponseWriter, _ *http.Request) {
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

		go exec.Stream(remotecommand.StreamOptions{Stdin: pr, Stdout: &out, Stderr: &out, Tty: true})
		pw.Write([]byte("/list\n"))
		time.Sleep(3 * time.Second)
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
		if result == "" {
			result = "(no player list received)"
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(result + "\n"))
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

		pods, err := cli.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector, Limit: 1})
		if err != nil || len(pods.Items) == 0 {
			conn.WriteMessage(websocket.TextMessage, []byte("no pod"))
			return
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
		exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", attach.URL())
		if err != nil {
			conn.WriteMessage(websocket.TextMessage, []byte("attach init error"))
			return
		}

		go exec.Stream(remotecommand.StreamOptions{Stdin: pr, Stdout: &wsWriter{conn}, Stderr: &wsWriter{conn}, Tty: true})

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				pw.Close()
				break
			}
			pw.Write(msg)
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
