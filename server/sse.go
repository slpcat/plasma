package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"encoding/json"

	"github.com/mssola/user_agent"
	"github.com/openfresh/plasma/config"
	"github.com/openfresh/plasma/event"
	"github.com/openfresh/plasma/log"
	"github.com/openfresh/plasma/manager"
	"github.com/openfresh/plasma/metrics"
	"github.com/openfresh/plasma/pubsub"
)

func NewSSEServer(opt Option) *http.Server {
	return &http.Server{
		Handler: newHandler(opt),
	}
}

type sseHandler struct {
	clientManager *manager.ClientManager
	timer         *time.Ticker
	newClients    chan manager.Client
	removeClients chan manager.Client
	payloads      chan event.Payload
	pubsub        pubsub.PubSuber
	retry         int
	eventQuery    string
	accessLogger  *zap.Logger
	errorLogger   *zap.Logger
	config        config.Config
	mux           *http.ServeMux
}

func newHandler(opt Option) sseHandler {
	h := sseHandler{
		clientManager: manager.NewClientManager(opt.ErrorLogger),
		timer:         time.NewTicker(10 * time.Second),
		newClients:    make(chan manager.Client),
		removeClients: make(chan manager.Client),
		payloads:      make(chan event.Payload),
		pubsub:        opt.PubSuber,
		retry:         opt.Config.SSE.Retry,
		eventQuery:    opt.Config.SSE.EventQuery,
		accessLogger:  opt.AccessLogger,
		errorLogger:   opt.ErrorLogger,
		config:        opt.Config,
	}
	h.pubsub.Subscribe(func(payload event.Payload) {
		h.payloads <- payload
	})
	h.Run()

	return h
}

const heartBeatEvent = "heartbeat"

func (h sseHandler) Run() {
	go func() {
		for {
			select {
			case client := <-h.newClients:
				h.clientManager.AddClient(client)
				metrics.IncConnection()
			case client := <-h.removeClients:
				h.clientManager.RemoveClient(client)
				metrics.DecConnection()
			case payload := <-h.payloads:
				h.clientManager.SendPayload(payload)
			case <-h.timer.C:
				h.clientManager.SendHeartBeat()
			}
		}
	}()
}

func isNotSupportSSE(u string) bool {
	ua := user_agent.New(u)

	name, _ := ua.Browser()

	switch name {
	case "Internet Explorer":
		return true
	case "Edge":
		return true
	}

	return false
}

func (h sseHandler) events(w http.ResponseWriter, r *http.Request) {
	eventRequestsQuery, ok := r.URL.Query()[h.eventQuery]
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	lastEvnetID := 0
	if id := r.Header.Get("HTTP_LAST_EVENT_ID"); id != "" {
		if i, err := strconv.Atoi(id); err == nil {
			lastEvnetID = i
		}
	} else if id, ok := r.URL.Query()["lastEventId"]; ok {
		if i, err := strconv.Atoi(id[0]); err == nil {
			lastEvnetID = i
		}
	}

	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "not streaming support", http.StatusInternalServerError)
		return
	}

	if len(eventRequestsQuery) == 0 || eventRequestsQuery[0] == "" {
		http.Error(w, "event query can't be empty", http.StatusBadRequest)
		return
	}

	// NOTE: eventRequestQuery[0] ex) 'program:1234:poll,program:1234:views'
	eventRequests := strings.Split(eventRequestsQuery[0], ",")

	if isNotSupportSSE(r.UserAgent()) {
		eventRequests = append(eventRequests, heartBeatEvent)
	}

	client := manager.NewClient(eventRequests)
	h.newClients <- client

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", h.config.Origin)

	var notify <-chan bool
	notifier, ok := w.(http.CloseNotifier)
	if ok {
		notify = notifier.CloseNotify()
	}
	fmt.Fprintf(w, "retry: %d\n", h.retry)
	for {
		select {
		case pl, open := <-client.ReceivePayload():
			if !open {
				return
			}
			eventType := pl.Meta.Type
			if eventType == heartBeatEvent {
				// NOTE: if use IE or Edge, need to send "comment" messages each 15-30 seconds, these messages will be used as heartbeat to detect disconnects
				// https://github.com/Yaffle/EventSource#server-side-requirements
				fmt.Fprint(w, ":heartbeat \n\n")
				f.Flush()
				lastEvnetID++
				continue
			}
			b, err := json.Marshal(pl)
			if err != nil {
				h.errorLogger.Error("failed to marshal event payload",
					zap.Error(err),
					zap.Object("payload", pl),
				)
				continue
			}
			fmt.Fprintf(w, "id: %d\n", lastEvnetID)
			fmt.Fprintf(w, "data: %s\n\n", string(b))
			f.Flush()
			lastEvnetID++
		case <-notify:
			h.removeClients <- client
			return
		}
	}

}

func (h sseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.accessLogger.Info("sse", log.HttpRequestToLogFields(r)...)
	h.events(w, r)
}
