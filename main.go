package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
)

const (
	linesToKeep   = 10
	chunkSize     = int64(1024)
	clientBufSize = 256
)

type Ring struct {
	mu    sync.RWMutex
	buf   []string
	size  int
	index int
	full  bool
}

func NewRing(n int) *Ring {
	return &Ring{buf: make([]string, n), size: n}
}

func (r *Ring) Push(line string) {
	r.mu.Lock()
	r.buf[r.index] = line
	r.index = (r.index + 1) % r.size
	if r.index == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

func (r *Ring) Snapshot() []string {
	if !r.full {
		return append([]string{}, r.buf[:r.index]...)
	}
	res := make([]string, 0, r.size)
	res = append(res, r.buf[r.index:]...)
	res = append(res, r.buf[:r.index]...)
	return res
}

func ReadLastN(filepath string, linesToRead int, chunkSize int64) ([]string, error) {
	f, err := os.Open(filepath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := stat.Size()
	if size == 0 {
		return nil, nil
	}

	var offset = size
	var data []byte

	for offset > 0 && bytes.Count(data, []byte{'\n'}) <= linesToRead {
		if offset < chunkSize {
			chunkSize = offset
		}
		offset -= chunkSize

		buf := make([]byte, chunkSize)
		n, _ := f.ReadAt(buf, offset)
		if n > 0 {
			data = append(buf[:n], data...)
		}
	}

	lines := bytes.Split(data, []byte{'\n'})

	res := make([]string, 0, linesToRead)
	for i := len(lines) - 1; i >= 0 && len(res) < linesToRead; i-- {
		line := strings.TrimSpace(string(lines[i]))
		if line == "" {
			continue
		}
		res = append(res, line)
	}
	// reverse
	for i, j := 0, len(res)-1; i < j; i, j = i+1, j-1 {
		res[i], res[j] = res[j], res[i]
	}
	return res, nil
}

type Client struct {
	conn       *websocket.Conn
	out        chan string
	unregister func(*Client)
	once       sync.Once
}

func (c *Client) Close() {
	c.once.Do(func() {
		c.unregister(c)
	})
}

var (
	registerCh   = make(chan *Client)
	unregisterCh = make(chan *Client)
	broadcastCh  = make(chan string, 1024)
)

func broadcaster(r *Ring) {
	clients := make(map[*Client]bool)
	for {
		select {
		case line := <-broadcastCh:
			for c := range clients {
				select {
				case c.out <- line:
				default:
					go func(cl *Client) { cl.Close() }(c)
				}
			}
		case c := <-registerCh:
			snap := r.Snapshot()
			for _, s := range snap {
				select {
				case c.out <- s:
				default:
					go func(cl *Client) { cl.Close() }(c)
				}
			}
			clients[c] = true
		case c := <-unregisterCh:
			if _, ok := clients[c]; ok {
				delete(clients, c)
				close(c.out)
			}
		}
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func serveWS() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsConn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			http.Error(w, "upgrade failed", http.StatusBadRequest)
			return
		}
		client := &Client{
			conn: wsConn,
			out:  make(chan string, clientBufSize),
			unregister: func(c *Client) {
				unregisterCh <- c
			},
		}

		go func(c *Client) {
			for {
				msg, _ := <-c.out
				if err := c.conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
					c.Close()
					return
				}
			}
		}(client)

		// register with broadcaster (broadcaster will send snapshot then add)
		registerCh <- client
	}
}

func startWatcher(filepath string, ring *Ring) {
	stat, err := os.Stat(filepath)
	var offset int64 = 0
	if err == nil {
		offset = stat.Size()
	}

	partial := []byte{}

	// readNew reads appended bytes since offset, extracts complete lines,
	// pushes them to ring and broadcastCh.
	readNew := func() {
		f, err := os.Open(filepath)
		if err != nil {
			return
		}
		defer f.Close()

		st, err := f.Stat()
		if err != nil {
			return
		}
		newSize := st.Size()
		if newSize <= offset {
			return
		}

		n64 := newSize - offset
		buf := make([]byte, int(n64))
		nread, _ := f.ReadAt(buf, offset)
		offset += int64(nread)

		partial = append(partial, buf[:nread]...)
		parts := bytes.Split(partial, []byte{'\n'})

		for i := 0; i < len(parts)-1; i++ {
			line := strings.TrimSpace(string(parts[i]))
			if line == "" {
				continue
			}
			ring.Push(line)
			select {
			case broadcastCh <- line:
			default:
			}
		}

		incmp := strings.TrimSpace(string(parts[len(parts)-1]))
		if len(incmp) > 0 {
			ring.Push(incmp)
			select {
			case broadcastCh <- incmp:
			default:
			}
			partial = []byte{}
		}
	}

	// set up fsnotify
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Println("fsnotify.NewWatcher error:", err)
		return
	}
	if err := watcher.Add(filepath); err != nil {
		_ = watcher.Add(".") // if filepath not found, scan the directory for it
	}

	go func() {
		readNew() // poll once at start to capture writes during startup

		for {
			select {
			case ev := <-watcher.Events:
				if ev.Op&fsnotify.Write == fsnotify.Write {
					// small debounce so writes coalesced
					time.Sleep(20 * time.Millisecond)
					readNew()
				}
			case err := <-watcher.Errors:
				log.Println("watcher error:", err)
			}
		}
	}()
}

func serveLogPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "log.html")
}

func main() {
	filepath := "temp.log"
	if len(os.Args) > 1 {
		filepath = os.Args[1]
	}

	// bootstrap last N lines
	ring := NewRing(linesToKeep)
	if last, err := ReadLastN(filepath, linesToKeep, chunkSize); err == nil && len(last) > 0 {
		for _, l := range last {
			ring.Push(l)
		}
	}

	go broadcaster(ring)

	// watcher that pushes incoming lines to broadcastCh and ring
	startWatcher(filepath, ring)

	http.HandleFunc("/log", serveLogPage)
	http.HandleFunc("/ws", serveWS())

	addr := ":8080"
	fmt.Println("Listening on", addr, "serving /log and /ws — watching", filepath)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
