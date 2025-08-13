// main.go
// Go rewrite of the FastAPI backend: Fiber (HTTP), Redis (go-redis v9), WebSockets, session lifecycle & metrics.
// Features:
// - POST /api/sessions            -> create automation session
// - GET  /api/sessions/:id        -> fetch single session
// - GET  /api/sessions            -> list active sessions
// - PATCH /api/sessions/:id       -> update session (status/progress)
// - POST /api/sessions/:id/cancel -> cancel session
// - GET  /api/metrics             -> latest metrics
// - GET  /healthz                 -> liveness
// - WS   /ws                      -> subscribe to events (session_* and metrics)
//
// Env vars:
//   PORT=8080
//   REDIS_URL=redis://localhost:6379
//   CORS_ORIGINS=*
//
// Build & Run:
//   go mod init webauto
//   go get github.com/gofiber/fiber/v2 github.com/gofiber/websocket/v2 github.com/redis/go-redis/v9 github.com/google/uuid
//   go run .
//
// Notes:
// - This is a single-file app for clarity; you can split into packages later.
// - Includes a background simulator that advances session progress and publishes events.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	websock "github.com/gofiber/websocket/v2"
	"github.com/google/uuid"
	redis "github.com/redis/go-redis/v9"
)

// ====== Types ======

type BehaviorPattern string

type SessionStatus string

const (
	BehaviorCasual    BehaviorPattern = "casual"
	BehaviorFocused   BehaviorPattern = "focused"
	BehaviorExplorer  BehaviorPattern = "explorer"
	BehaviorScanner   BehaviorPattern = "scanner"
	BehaviorResearch  BehaviorPattern = "researcher"

	StatusPending   SessionStatus = "pending"
	StatusRunning   SessionStatus = "running"
	StatusCompleted SessionStatus = "completed"
	StatusFailed    SessionStatus = "failed"
	StatusCancelled SessionStatus = "cancelled"
)

type AutomationRequest struct {
	URL             string           `json:"url"`
	BehaviorPattern BehaviorPattern  `json:"behavior_pattern"`
	MaxInteractions int              `json:"max_interactions"`
	Headless        bool             `json:"headless"`
	TimeoutSec      int              `json:"timeout"`
	CustomScript    *string          `json:"custom_script"`
	Proxy           *string          `json:"proxy"`
}

type Session struct {
	SessionID       string           `json:"session_id"`
	Status          SessionStatus    `json:"status"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	BehaviorPattern BehaviorPattern  `json:"behavior_pattern"`
	URL             string           `json:"url"`
	MaxInteractions int              `json:"max_interactions"`
	Headless        bool             `json:"headless"`
	TimeoutSec      int              `json:"timeout"`
	Progress        int              `json:"progress"`
	Metrics         map[string]any   `json:"metrics"`
	Error           *string          `json:"error"`
}

type MetricsData struct {
	Timestamp        time.Time `json:"timestamp"`
	CPUUsage         float64   `json:"cpu_usage"`
	MemoryUsage      float64   `json:"memory_usage"`
	ActiveSessions   int       `json:"active_sessions"`
	SuccessRate      float64   `json:"success_rate"`
	AvgResponseMs    float64   `json:"avg_response_time"`
	TotalInteractions int      `json:"total_interactions"`
}

// ====== Redis Manager ======

type RedisManager struct {
	client *redis.Client
}

func NewRedisManager(ctx context.Context, urlStr string) (*RedisManager, error) {
	if urlStr == "" {
		urlStr = "redis://localhost:6379"
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}

	addr := u.Host
	pass := ""
	if u.User != nil {
		p, _ := u.User.Password()
		pass = p
	}
	opt := &redis.Options{Addr: addr, Password: pass, DB: 0}
	client := redis.NewClient(opt)
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return &RedisManager{client: client}, nil
}

func (r *RedisManager) Close() error { return r.client.Close() }

// Keys & channels
const (
	sessionsPrefix   = "automation:sessions:"
	activeSetKey     = "automation:active_sessions"
	metricsKey       = "automation:metrics:latest"
	eventsChannel    = "automation:events"
)

// ====== Session Store ======

type SessionStore struct {
	r *RedisManager
}

func NewSessionStore(r *RedisManager) *SessionStore { return &SessionStore{r: r} }

func (s *SessionStore) Save(ctx context.Context, sess *Session) error {
	b, _ := json.Marshal(sess)
	// Expire one hour after timeout
	expire := time.Duration(sess.TimeoutSec+3600) * time.Second
	if err := s.r.client.Set(ctx, sessionsPrefix+sess.SessionID, string(b), expire).Err(); err != nil {
		return err
	}
	if err := s.r.client.SAdd(ctx, activeSetKey, sess.SessionID).Err(); err != nil {
		return err
	}
	return nil
}

func (s *SessionStore) Get(ctx context.Context, id string) (*Session, error) {
	res, err := s.r.client.Get(ctx, sessionsPrefix+id).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) { return nil, fiber.ErrNotFound }
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal([]byte(res), &sess); err != nil { return nil, err }
	return &sess, nil
}

func (s *SessionStore) Update(ctx context.Context, id string, update func(*Session) error) (*Session, error) {
	sess, err := s.Get(ctx, id)
	if err != nil { return nil, err }
	if err := update(sess); err != nil { return nil, err }
	sess.UpdatedAt = time.Now().UTC()
	if err := s.Save(ctx, sess); err != nil { return nil, err }
	return sess, nil
}

func (s *SessionStore) List(ctx context.Context) ([]*Session, error) {
	ids, err := s.r.client.SMembers(ctx, activeSetKey).Result()
	if err != nil { return nil, err }
	out := make([]*Session, 0, len(ids))
	for _, id := range ids {
		if sess, err := s.Get(ctx, id); err == nil {
			out = append(out, sess)
		}
	}
	return out, nil
}

func (s *SessionStore) RemoveActive(ctx context.Context, id string) error {
	return s.r.client.SRem(ctx, activeSetKey, id).Err()
}

// ====== Event Hub (WebSocket) ======

type WSClient struct {
	Conn *websock.Conn
	Mu   sync.Mutex
}

type Hub struct {
	clients map[*WSClient]struct{}
	mu      sync.RWMutex
}

func NewHub() *Hub { return &Hub{clients: map[*WSClient]struct{}{}} }

func (h *Hub) Add(c *WSClient) {
	h.mu.Lock(); defer h.mu.Unlock()
	h.clients[c] = struct{}{}
}

func (h *Hub) Remove(c *WSClient) {
	h.mu.Lock(); defer h.mu.Unlock()
	delete(h.clients, c)
}

func (h *Hub) Broadcast(v any) {
	b, _ := json.Marshal(v)
	h.mu.RLock(); defer h.mu.RUnlock()
	for c := range h.clients {
		c.Mu.Lock()
		_ = c.Conn.WriteMessage(1, b)
		c.Mu.Unlock()
	}
}

// ====== Utilities ======

func validateRequest(req *AutomationRequest) error {
	if req.URL == "" { return fmt.Errorf("url is required") }
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		return fmt.Errorf("url must start with http:// or https://")
	}
	if req.BehaviorPattern == "" { req.BehaviorPattern = BehaviorFocused }
	if req.MaxInteractions <= 0 || req.MaxInteractions > 50 { req.MaxInteractions = 5 }
	if req.TimeoutSec < 30 || req.TimeoutSec > 3600 { req.TimeoutSec = 300 }
	return nil
}

func nowUTC() time.Time { return time.Now().UTC() }

// ====== Metrics ======

type Metrics struct {
	mu               sync.RWMutex
	last MetricsData
}

func (m *Metrics) Set(md MetricsData) { m.mu.Lock(); m.last = md; m.mu.Unlock() }
func (m *Metrics) Get() MetricsData { m.mu.RLock(); defer m.mu.RUnlock(); return m.last }

// ====== Main ======

func main() {
	ctx := context.Background()

	redisURL := os.Getenv("REDIS_URL")
	rm, err := NewRedisManager(ctx, redisURL)
	if err != nil { log.Fatalf("Redis connect failed: %v", err) }
	defer rm.Close()

	store := NewSessionStore(rm)
	hub := NewHub()
	metrics := &Metrics{}

	app := fiber.New()
	app.Use(logger.New())
	app.Use(cors.New(cors.Config{AllowOrigins: origins(), AllowHeaders: "*", AllowCredentials: true}))

	app.Get("/healthz", func(c *fiber.Ctx) error { return c.SendString("ok") })

	api := app.Group("/api")

	// Create session
	api.Post("/sessions", func(c *fiber.Ctx) error {
		var req AutomationRequest
		if err := c.BodyParser(&req); err != nil { return fiber.NewError(fiber.StatusBadRequest, err.Error()) }
		if err := validateRequest(&req); err != nil { return fiber.NewError(fiber.StatusBadRequest, err.Error()) }

		id := "sess_" + uuid.NewString()[:12]
		sess := &Session{
			SessionID:       id,
			Status:          StatusPending,
			CreatedAt:       nowUTC(),
			UpdatedAt:       nowUTC(),
			BehaviorPattern: req.BehaviorPattern,
			URL:             req.URL,
			MaxInteractions: req.MaxInteractions,
			Headless:        req.Headless,
			TimeoutSec:      req.TimeoutSec,
			Progress:        0,
			Metrics:         map[string]any{},
		}
		if err := store.Save(ctx, sess); err != nil { return fiber.NewError(fiber.StatusInternalServerError, err.Error()) }

		// Publish event
		publish(rm, map[string]any{"event": "session_created", "session_id": id})

		// kick off background simulator (replace with real automation engine)
		go simulateSession(ctx, rm, store, hub, id)

		return c.Status(fiber.StatusCreated).JSON(sess)
	})

	// Get session by id
	api.Get("/sessions/:id", func(c *fiber.Ctx) error {
		id := c.Params("id")
		sess, err := store.Get(ctx, id)
		if err != nil { return err }
		return c.JSON(sess)
	})

	// List sessions
	api.Get("/sessions", func(c *fiber.Ctx) error {
		list, err := store.List(ctx)
		if err != nil { return fiber.NewError(fiber.StatusInternalServerError, err.Error()) }
		return c.JSON(list)
	})

	// Update session (status/progress)
	api.Patch("/sessions/:id", func(c *fiber.Ctx) error {
		id := c.Params("id")
		var body map[string]any
		if err := json.Unmarshal(c.Body(), &body); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid json")
		}
		sess, err := store.Update(ctx, id, func(s *Session) error {
			if v, ok := body["status"].(string); ok { s.Status = SessionStatus(v) }
			if v, ok := body["progress"].(float64); ok {
				p := int(v); if p < 0 { p = 0 }; if p > 100 { p = 100 }; s.Progress = p
			}
			if v, ok := body["error"].(string); ok { s.Error = &v }
			return nil
		})
		if err != nil { return err }
		publish(rm, map[string]any{"event": "session_updated", "session_id": id, "status": sess.Status, "progress": sess.Progress})
		return c.JSON(sess)
	})

	// Cancel
	api.Post("/sessions/:id/cancel", func(c *fiber.Ctx) error {
		id := c.Params("id")
		_, err := store.Update(ctx, id, func(s *Session) error {
			s.Status = StatusCancelled
			s.Progress = min(100, s.Progress)
			return nil
		})
		if err != nil { return err }
		publish(rm, map[string]any{"event": "session_cancelled", "session_id": id})
		return c.JSON(fiber.Map{"ok": true})
	})

	// Metrics
	api.Get("/metrics", func(c *fiber.Ctx) error { return c.JSON(metrics.Get()) })

	// WebSocket endpoint
	app.Get("/ws", websock.New(func(c *websock.Conn) {
		client := &WSClient{Conn: c}
		hub.Add(client)
		defer func() { hub.Remove(client); _ = c.Close() }()
		for {
			// Read to detect close; ignore messages
			if _, _, err := c.ReadMessage(); err != nil { break }
		}
	}))

	// Background: subscribe Redis events and re-broadcast to WS
	go func() {
		sub := rm.client.Subscribe(ctx, eventsChannel)
		ch := sub.Channel()
		for msg := range ch {
			var v any
			_ = json.Unmarshal([]byte(msg.Payload), &v)
			hub.Broadcast(v)
		}
	}()

	// Background: metrics generator (replace with real system probes)
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		var tick int
		for range t.C {
			tick++
			active, _ := rm.client.SCard(ctx, activeSetKey).Result()
			md := MetricsData{
				Timestamp:         time.Now().UTC(),
				CPUUsage:          10 + float64((tick*7)%60),
				MemoryUsage:       30 + float64((tick*5)%50),
				ActiveSessions:    int(active),
				SuccessRate:       0.7 + 0.3*float64((tick%10))/10.0,
				AvgResponseMs:     120 + float64((tick*13)%200),
				TotalInteractions: tick * 3,
			}
			metrics.Set(md)
			b, _ := json.Marshal(md)
			_ = rm.client.Set(ctx, metricsKey, b, 30*time.Second).Err()
			publish(rm, map[string]any{"event": "metrics", "data": md})
		}
	}()

	port := os.Getenv("PORT")
	if port == "" { port = "8080" }
	log.Printf("listening on :%s", port)
	if err := app.Listen(":" + port); err != nil { log.Fatal(err) }
}

// ====== Helpers ======

func publish(rm *RedisManager, v any) {
	b, _ := json.Marshal(v)
	_ = rm.client.Publish(context.Background(), eventsChannel, b).Err()
}

func origins() string {
	if o := os.Getenv("CORS_ORIGINS"); o != "" { return o }
	return "*"
}

func min(a, b int) int { if a < b { return a }; return b }

// ====== Demo Session Simulator ======

func simulateSession(ctx context.Context, rm *RedisManager, store *SessionStore, hub *Hub, id string) {
	// Start running
	_, err := store.Update(ctx, id, func(s *Session) error {
		s.Status = StatusRunning
		return nil
	})
	if err != nil { return }
	publish(rm, map[string]any{"event": "session_started", "session_id": id})

	deadline := time.Now().Add(10 * time.Second)
	step := 0
	for time.Now().Before(deadline) {
		step++
		time.Sleep(1 * time.Second)
		progress := min(100, step*10)
		_, err := store.Update(ctx, id, func(s *Session) error {
			s.Progress = progress
			// fake some metrics per session
			if s.Metrics == nil { s.Metrics = map[string]any{} }
			s.Metrics["last_step_ms"] = 800 + step*25
			return nil
		})
		if err != nil { break }
		publish(rm, map[string]any{"event": "session_progress", "session_id": id, "progress": progress})
	}

	// Finish
	_, _ = store.Update(ctx, id, func(s *Session) error {
		s.Status = StatusCompleted
		s.Progress = 100
		return nil
	})
	_ = store.RemoveActive(ctx, id)
	publish(rm, map[string]any{"event": "session_completed", "session_id": id})
}
