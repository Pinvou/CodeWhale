package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var models = []string{"pinvou-mock", "pinvou-mock-rate-limit", "pinvou-mock-disconnect", "pinvou-mock-missing-usage"}

func main() {
	if err := httpserver.ConfigureProcessLogging("mock-newapi"); err != nil {
		log.Fatal(err)
	}
	apiKey := os.Getenv("MOCK_NEWAPI_KEY")
	if len(apiKey) < 16 {
		log.Fatal("MOCK_NEWAPI_KEY must contain at least 16 characters")
	}
	router := httpserver.NewRouter()
	router.Use(authenticate(apiKey))
	router.GET("/v1/models", httpserver.AdaptFunc(listModels))
	router.POST("/v1/chat/completions", httpserver.AdaptFunc(chatCompletions))
	if err := httpserver.RunWithApplication("mock-newapi", router, func(context.Context) error { return nil }); err != nil {
		log.Fatal(err)
	}
}

func authenticate(apiKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := strings.TrimPrefix(c.Request.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(apiKey) || subtle.ConstantTimeCompare([]byte(provided), []byte(apiKey)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "unauthorized"}})
			return
		}
		c.Next()
	}
}

func listModels(w http.ResponseWriter, _ *http.Request) {
	items := make([]map[string]string, 0, len(models))
	for _, model := range models {
		items = append(items, map[string]string{"id": model, "object": "model"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items})
}

func chatCompletions(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&request) != nil || !contains(models, request.Model) {
		http.Error(w, `{"error":{"message":"invalid request"}}`, http.StatusBadRequest)
		return
	}
	if request.Model == "pinvou-mock-rate-limit" {
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
		return
	}
	id := "chatcmpl-" + uuid.Must(uuid.NewV7()).String()
	usage := map[string]any{"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16, "prompt_tokens_details": map[string]int{"cached_tokens": 2}}
	if !request.Stream {
		response := map[string]any{"id": id, "object": "chat.completion", "model": request.Model, "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": "mock response"}, "finish_reason": "stop"}}}
		if request.Model != "pinvou-mock-missing-usage" {
			response["usage"] = usage
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	writeEvent(w, flusher, map[string]any{"id": id, "object": "chat.completion.chunk", "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant", "content": "mock "}}}})
	if request.Model == "pinvou-mock-disconnect" {
		return
	}
	writeEvent(w, flusher, map[string]any{"id": id, "object": "chat.completion.chunk", "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "response"}, "finish_reason": "stop"}}})
	if request.Model != "pinvou-mock-missing-usage" {
		writeEvent(w, flusher, map[string]any{"id": id, "object": "chat.completion.chunk", "model": request.Model, "choices": []any{}, "usage": usage})
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func writeEvent(w http.ResponseWriter, flusher http.Flusher, payload any) {
	data, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher != nil {
		flusher.Flush()
	}
	time.Sleep(5 * time.Millisecond)
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
