package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ridge/must/v2"
	"github.com/tiktoken-go/tokenizer"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
)

const openaiURL = "https://api.openai.com"

type completionRequestBody struct {
	Model    string
	Messages []struct {
		Content string
	}
	Suffix string
	Stream bool
}

type completionResponseBody struct {
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	}
}

type completionResponseStreamedBody struct {
	Choices []struct {
		Delta struct {
			Content string
		}
	}
}

const timeFmt = `2006-01-02 15:04:05.000`

func reqPrint(r *http.Request, prefix string, fmt string, args ...any) {
	log.Printf(prefix+"%s %21s "+fmt, append([]any{
		time.Now().UTC().Format(timeFmt),
		r.RemoteAddr,
	}, args...)...)
}

func logInfo(r *http.Request, fmt string, args ...any) {
	reqPrint(r, "INF ", fmt, args...)
}

func logError(r *http.Request, fmt string, args ...any) {
	reqPrint(r, "ERR ", fmt, args...)
}

func getMessageFromSSE(sseMsg string) string {
	var msg string
	for _, line := range strings.Split(sseMsg, "\n") {
		if strings.HasPrefix(line, "data:") {
			msg += strings.TrimSpace(line[5:])
		}
	}
	return msg
}

func proxySSEResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, conn *sqlite.Conn, userName string, userID int64, projectName string, projectID int64, modelID int64, crb completionRequestBody, tk tokenizer.Codec) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		logError(r, "Unable to get flusher for response")
		http.Error(w, "Streaming setup failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	nTokens := 0
	for _, message := range crb.Messages {
		ids, _, err := tk.Encode(message.Content)
		if err != nil {
			logError(r, "Failed to tokenize prompt for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
			http.Error(w, "failed to tokenize prompt", http.StatusBadGateway)
			return
		}
		nTokens += len(ids)
	}

	logInfo(r, "Tokenized prompt for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %d tokens", userName, userID, projectName, projectID, crb.Model, modelID, nTokens)

	// Read the response line-by-line and send it to the client
	reader := bufio.NewReader(resp.Body)
	for {
		var msg string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				logError(r, "Failed to read response body for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
				http.Error(w, "failed to read response", http.StatusBadGateway)
				return
			}
			fmt.Fprint(w, line)
			flusher.Flush()

			if line == "\n" {
				// End of message
				break
			}

			if strings.HasPrefix(line, "data:") {
				msg += strings.TrimSpace(line[5:])
			}
		}

		if msg == "[DONE]" {
			break
		}

		fmt.Printf("msg: %q\n", msg)

		var respBody completionResponseStreamedBody
		if err := json.Unmarshal([]byte(msg), &respBody); err != nil {
			logError(r, "Failed to unmarshal response body for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
			http.Error(w, "failed to unmarshal response", http.StatusBadGateway)
			return
		}
		if len(respBody.Choices) != 1 {
			logError(r, "0 or more than 1 choices in response body for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d)", userName, userID, projectName, projectID, crb.Model, modelID)
			http.Error(w, "0 or more than 1 choices in response", http.StatusBadGateway)
			return
		}

		ids, _, err := tk.Encode(respBody.Choices[0].Delta.Content)
		if err != nil {
			logError(r, "Failed to tokenize message for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
			http.Error(w, "failed to tokenize message", http.StatusBadGateway)
			return
		}
		nTokens += len(ids)
	}

	logInfo(r, "SSE response read. user %q (ID=%d), project %q (ID=%d), model %q (ID=%d), tokens %d", userName, userID, projectName, projectID, crb.Model, modelID, nTokens)

	if err := saveUsage(conn, modelID, projectID, nTokens); err != nil {
		logError(r, "Failed to save usage for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d), tokens %d: %v", userName, userID, projectName, projectID, crb.Model, modelID, nTokens, err)
	}
}

func proxyPlainResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, conn *sqlite.Conn, userName string, userID int64, projectName string, projectID int64, modelID int64, crb completionRequestBody) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logError(r, "Failed to read response body for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
		http.Error(w, "failed to read response", http.StatusBadGateway)
		return
	}

	var crespb completionResponseBody
	if err := json.Unmarshal(responseBody, &crespb); err != nil {
		logError(r, "Failed to parse response body for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
		http.Error(w, "failed to parse response", http.StatusBadGateway)
		return
	}

	logInfo(r, "200 response read. user %q (ID=%d), project %q (ID=%d), model %q (ID=%d), tokens %d", userName, userID, projectName, projectID, crb.Model, modelID, crespb.Usage.TotalTokens)

	if err := saveUsage(conn, modelID, projectID, crespb.Usage.TotalTokens); err != nil {
		logError(r, "Failed to save usage for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d), tokens %d: %v", userName, userID, projectName, projectID, crb.Model, modelID, crespb.Usage.TotalTokens, err)
	}

	if _, err := w.Write(responseBody); err != nil {
		logError(r, "Failed to write response body for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
	}

	logInfo(r, "200 response sent. user %q (ID=%d), project %q (ID=%d), model %q (ID=%d)", userName, userID, projectName, projectID, crb.Model, modelID)
}

func proxyRequest(w http.ResponseWriter, r *http.Request, client *http.Client, key string, pool *sqlitemigration.Pool) {
	if r.Method != http.MethodPost {
		logError(r, "Unexpected method %q", r.Method)
		http.Error(w, "Only POST requests are supported", http.StatusBadRequest)
		return
	}
	if r.URL.RawQuery != "" {
		logError(r, "Unexpected query %q", r.URL.RawQuery)
		http.Error(w, "Query parameters are not supported", http.StatusBadRequest)
		return
	}

	// We do not use request's context, as we want to count requests that were aborted by the client too.
	// Instead we use a new context with a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn := mustGetDB(ctx, pool)
	defer pool.Put(conn)

	reqKey := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	userID, userName, userFound, err := findUserByKey(conn, reqKey)
	if err != nil {
		logError(r, "Failed to find user by key: %v", err)
		http.Error(w, "Failed to find user", http.StatusInternalServerError)
		return
	}
	if !userFound {
		logError(r, "User not found by key %q", reqKey)
		http.Error(w, "Invalid API key", http.StatusUnauthorized)
		return
	}

	projectName := r.Header.Get("X-Project")
	if projectName == "" {
		projectName = "<default>"
	}

	projectID, err := getProjectID(conn, userID, projectName)
	if err != nil {
		logError(r, "Failed to get project ID for user %q (ID=%d), project %q: %v", userName, userID, projectName, err)
		http.Error(w, "failed to find project", http.StatusInternalServerError)
		return
	}

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		logError(r, "Failed to read request body for user %q (ID=%d), project %q (ID=%d): %v", userName, userID, projectName, projectID, err)
		http.Error(w, "failed to read request body", http.StatusInternalServerError)
		return
	}

	var crb completionRequestBody
	if err := json.Unmarshal(requestBody, &crb); err != nil {
		logError(r, "Failed to parse request body for user %q (ID=%d), project %q (ID=%d): %v", userName, userID, projectName, projectID, err)
		http.Error(w, "failed to parse request body", http.StatusBadRequest)
		return
	}

	tk, err := tokenizer.ForModel(tokenizer.Model(crb.Model))
	if err != nil {
		logError(r, "Invalid model %q requested by user %q (ID=%d), project %q (ID=%d): %v", crb.Model, userName, userID, projectName, projectID, err)
		http.Error(w, "failed to find model "+crb.Model, http.StatusBadRequest)
		return
	}

	modelID, err := getModelID(conn, crb.Model)
	if err != nil {
		logError(r, "Failed to get model ID for model %q, requested by user %q (ID=%d), project %q (ID=%d): %v", crb.Model, userName, userID, projectName, projectID, err)
		http.Error(w, "failed to get model "+crb.Model, http.StatusInternalServerError)
		return
	}

	logInfo(r, "Proxying. user %q (ID=%d), project %q (ID=%d), model %q (ID=%d)", userName, userID, projectName, projectID, crb.Model, modelID)

	req := must.OK1(http.NewRequestWithContext(ctx, http.MethodPost, openaiURL+"/v1/chat/completions", bytes.NewReader(requestBody)))
	req.Header = r.Header.Clone()
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)

	// Network failures etc.
	if err != nil {
		logError(r, "Failed to proxy request for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
		http.Error(w, fmt.Sprintf("Failed to read response from OpenAI: %v", err), http.StatusBadGateway)
		return
	}

	defer resp.Body.Close()

	h := w.Header()
	for k, vs := range resp.Header {
		h.Del(k)
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		if _, err := io.Copy(w, resp.Body); err != nil {
			logError(r, "Failed to write response body for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
		}

		logInfo(r, "Error response sent. %s, user %q (ID=%d), project %q (ID=%d), model %q (ID=%d)", resp.Status, userName, userID, projectName, projectID, crb.Model, modelID)
		return
	}

	if crb.Stream {
		proxySSEResponse(w, r, resp, conn, userName, userID, projectName, projectID, modelID, crb, tk)
	} else {
		proxyPlainResponse(w, r, resp, conn, userName, userID, projectName, projectID, modelID, crb)
	}
}

func serve(pool *sqlitemigration.Pool, listenURL string) {
	openaiKey := os.Getenv("OPENAI_KEY")
	client := &http.Client{}

	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		proxyRequest(w, r, client, openaiKey, pool)
	})

	if err := http.ListenAndServe(listenURL, nil); err != nil {
		log.Fatalf("failed to listen on %s: %v", listenURL, err)
	}
}
