package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Config struct {
	Port           string
	BaseURL        string
	TurnURL        string
	TurnUsername   string
	TurnCredential string
	OriginPatterns []string
}

type SignalMessage struct {
	Type  string          `json:"type"`
	Room  string          `json:"room"`
	Token string          `json:"token"`
	Data  json.RawMessage `json:"data"`
}

type Client struct {
	conn *websocket.Conn
	send chan []byte
}

type Room struct {
	id        string
	token     string
	clients   map[*Client]bool
	created   time.Time
	expiresAt time.Time
}

type Server struct {
	mu     sync.Mutex
	rooms  map[string]*Room
	config Config
}

func NewServer(config Config) *Server {
	return &Server{
		rooms:  make(map[string]*Room),
		config: config,
	}
}

func loadConfig() Config {
	config := Config{
		Port:           env("APP_PORT", "8008"),
		BaseURL:        strings.TrimRight(os.Getenv("APP_BASE_URL"), "/"),
		TurnURL:        os.Getenv("TURN_URL"),
		TurnUsername:   os.Getenv("TURN_USERNAME"),
		TurnCredential: os.Getenv("TURN_CREDENTIAL"),
		OriginPatterns: []string{
			"localhost:*",
			"http://localhost:*",
			"https://localhost:*",
			"127.0.0.1:*",
			"http://127.0.0.1:*",
			"https://127.0.0.1:*",
			"192.168.*:*",
			"http://192.168.*:*",
			"https://192.168.*:*",
			"*.ngrok-free.app",
			"*.ngrok-free.app:*",
			"https://*.ngrok-free.app",
			"https://*.ngrok-free.app:*",
			"*.ngrok.io",
			"*.ngrok.io:*",
			"https://*.ngrok.io",
			"https://*.ngrok.io:*",
		},
	}

	if config.BaseURL != "" {
		if parsed, err := url.Parse(config.BaseURL); err == nil && parsed.Host != "" {
			config.OriginPatterns = append(config.OriginPatterns, parsed.Scheme+"://"+parsed.Host)
		}
	}

	return config
}

func env(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	roomID := randomHex(16)
	token := randomHex(32)

	room := &Room{
		id:        roomID,
		token:     token,
		clients:   make(map[*Client]bool),
		created:   time.Now(),
		expiresAt: time.Now().Add(1 * time.Hour),
	}

	s.mu.Lock()
	s.rooms[roomID] = room
	s.mu.Unlock()

	resp := map[string]string{
		"room":  roomID,
		"token": token,
		"url":   "/?room=" + roomID + "&token=" + token,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) appConfig(w http.ResponseWriter, r *http.Request) {
	iceServers := []map[string]string{
		{
			"urls": "stun:stun.l.google.com:19302",
		},
		{
			"urls": "stun:global.stun.twilio.com:3478",
		},
	}

	if s.config.TurnURL != "" && s.config.TurnUsername != "" && s.config.TurnCredential != "" {
		iceServers = append(iceServers, map[string]string{
			"urls":       s.config.TurnURL,
			"username":   s.config.TurnUsername,
			"credential": s.config.TurnCredential,
		})
	}

	resp := map[string]any{
		"baseUrl":    s.config.BaseURL,
		"iceServers": iceServers,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	token := r.URL.Query().Get("token")

	if roomID == "" || token == "" {
		http.Error(w, "room and token required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	room, ok := s.rooms[roomID]
	if !ok || room.token != token || time.Now().After(room.expiresAt) {
		s.mu.Unlock()
		http.Error(w, "invalid or expired room", http.StatusForbidden)
		return
	}

	if len(room.clients) >= 2 {
		s.mu.Unlock()
		http.Error(w, "room is full", http.StatusForbidden)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.config.OriginPatterns,
	})
	if err != nil {
		s.mu.Unlock()
		log.Println("websocket accept failed:", err)
		return
	}

	client := &Client{
		conn: conn,
		send: make(chan []byte, 32),
	}

	room.clients[client] = true
	s.mu.Unlock()

	log.Println("client joined room:", roomID)
	s.broadcastToOther(roomID, client, []byte(`{"type":"peer-joined"}`))

	go s.writeLoop(r.Context(), client)

	s.readLoop(r.Context(), roomID, client)
}

func (s *Server) readLoop(ctx context.Context, roomID string, client *Client) {
	defer func() {
		s.mu.Lock()
		if room, ok := s.rooms[roomID]; ok {
			delete(room.clients, client)
			if len(room.clients) == 0 {
				delete(s.rooms, roomID)
			}
		}
		s.mu.Unlock()

		close(client.send)
		_ = client.conn.Close(websocket.StatusNormalClosure, "closed")
	}()

	for {
		_, data, err := client.conn.Read(ctx)
		if err != nil {
			return
		}

		var msg SignalMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		if msg.Room != roomID {
			continue
		}

		s.broadcastToOther(roomID, client, data)
	}
}

func (s *Server) writeLoop(ctx context.Context, client *Client) {
	for {
		select {
		case <-ctx.Done():
			return

		case msg, ok := <-client.send:
			if !ok {
				return
			}

			err := client.conn.Write(ctx, websocket.MessageText, msg)
			if err != nil {
				return
			}
		}
	}
}

func (s *Server) broadcastToOther(roomID string, sender *Client, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	room, ok := s.rooms[roomID]
	if !ok {
		return
	}

	for client := range room.clients {
		if client != sender {
			select {
			case client.send <- data:
			default:
			}
		}
	}
}

func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()

		s.mu.Lock()
		for id, room := range s.rooms {
			if now.After(room.expiresAt) {
				delete(s.rooms, id)
			}
		}
		s.mu.Unlock()
	}
}

func main() {
	config := loadConfig()
	srv := NewServer(config)
	go srv.cleanupLoop()

	http.HandleFunc("/api/config", srv.appConfig)
	http.HandleFunc("/api/room", srv.createRoom)
	http.HandleFunc("/ws", srv.ws)

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	log.Println("server started on http://0.0.0.0:" + config.Port)
	if config.BaseURL != "" {
		log.Println("public URL:", config.BaseURL)
	}
	log.Fatal(http.ListenAndServe("0.0.0.0:"+config.Port, nil))
}
