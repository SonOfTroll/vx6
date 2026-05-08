package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type appMode string

const (
	modeOpen appMode = "open"
	modeOrg  appMode = "org"
)

type peerContact struct {
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name"`
	Address   string `json:"address"`
	Secret    string `json:"secret"`
	AddedAt   string `json:"added_at"`
	Accepted  bool   `json:"accepted"`
	RequestID string `json:"request_id"`
}

type messageEnvelope struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	AckFor    string `json:"ack_for,omitempty"`
	MediaName string `json:"media_name,omitempty"`
	MediaSize int64  `json:"media_size,omitempty"`
	MediaSHA  string `json:"media_sha,omitempty"`
	GroupID   string `json:"group_id,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
	CreatedAt string `json:"created_at"`
	Nonce     string `json:"nonce"`
	Cipher    string `json:"cipher"`
}

type conversationLedger struct {
	PairKey   string            `json:"pair_key"`
	UpdatedAt string            `json:"updated_at"`
	Messages  []messageEnvelope `json:"messages"`
}

type chatMessage struct {
	Text string `json:"text"`
}

type friendRequest struct {
	RequestID string `json:"request_id"`
	FromID    string `json:"from_id"`
	FromName  string `json:"from_name"`
	Address   string `json:"address"`
	Secret    string `json:"secret"`
	CreatedAt string `json:"created_at"`
}

type groupRoom struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Secret    string   `json:"secret"`
	Members   []string `json:"members"`
	CreatedAt string   `json:"created_at"`
}

type queuedMessage struct {
	ContactID string          `json:"contact_id"`
	Envelope  messageEnvelope `json:"envelope"`
	Retries   int             `json:"retries"`
	NextRetry string          `json:"next_retry"`
}

type localState struct {
	Unread       map[string]int       `json:"unread"`
	SeenMessage  map[string]bool      `json:"seen_message"`
	Pending      map[string]bool      `json:"pending"`
	Outbox       []queuedMessage      `json:"outbox"`
	LastSyncAt   string               `json:"last_sync_at"`
	ActiveGroups map[string]groupRoom `json:"active_groups"`
}

func pairKey(a, b string) string {
	ids := []string{a, b}
	sort.Strings(ids)
	return "vx6chat/conv/" + ids[0] + "/" + ids[1]
}

func requestKey(toNodeID string) string {
	return "vx6chat/request/" + toNodeID
}

func groupKey(groupID string) string {
	return "vx6chat/group/" + groupID
}

func marshalJSON(v any) []byte {
	out, _ := json.Marshal(v)
	return out
}

func sealMessage(secret string, plain chatMessage, from, to, kind string) (messageEnvelope, error) {
	raw, err := json.Marshal(plain)
	if err != nil {
		return messageEnvelope{}, err
	}
	key := sha256.Sum256([]byte("vx6-chat-secret\n" + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return messageEnvelope{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return messageEnvelope{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return messageEnvelope{}, err
	}
	ciphertext := gcm.Seal(nil, nonce, raw, []byte(from+"\n"+to))
	sum := sha256.Sum256(append(nonce, ciphertext...))
	return messageEnvelope{
		ID:        base64.RawURLEncoding.EncodeToString(sum[:12]),
		Type:      kind,
		From:      from,
		To:        to,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Nonce:     base64.RawURLEncoding.EncodeToString(nonce),
		Cipher:    base64.RawURLEncoding.EncodeToString(ciphertext),
	}, nil
}

func makeAckMessage(ackedID, from, to string) messageEnvelope {
	return messageEnvelope{
		ID:        base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("ack-%s-%d", ackedID, time.Now().UnixNano()))),
		Type:      "ack",
		AckFor:    ackedID,
		From:      from,
		To:        to,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func openMessage(secret string, env messageEnvelope) (chatMessage, error) {
	key := sha256.Sum256([]byte("vx6-chat-secret\n" + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return chatMessage{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return chatMessage{}, err
	}
	nonce, err := base64.RawURLEncoding.DecodeString(env.Nonce)
	if err != nil {
		return chatMessage{}, err
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(env.Cipher)
	if err != nil {
		return chatMessage{}, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, []byte(env.From+"\n"+env.To))
	if err != nil {
		return chatMessage{}, err
	}
	var msg chatMessage
	if err := json.Unmarshal(plain, &msg); err != nil {
		return chatMessage{}, err
	}
	return msg, nil
}

func randomSecret() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func inviteLink(nodeID, nodeName, addr, secret string) string {
	req := friendRequest{
		RequestID: base64.RawURLEncoding.EncodeToString([]byte(nodeID))[:8] + fmt.Sprintf("%d", time.Now().UnixNano()%100000),
		FromID:    nodeID,
		FromName:  nodeName,
		Address:   addr,
		Secret:    secret,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return "vx6chat://invite/" + base64.RawURLEncoding.EncodeToString(marshalJSON(req))
}

func parseInviteLink(link string) (friendRequest, error) {
	const p = "vx6chat://invite/"
	if !strings.HasPrefix(strings.TrimSpace(link), p) {
		return friendRequest{}, fmt.Errorf("invalid invite")
	}
	raw := strings.TrimPrefix(strings.TrimSpace(link), p)
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return friendRequest{}, err
	}
	var req friendRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return friendRequest{}, err
	}
	if req.FromID == "" || req.Address == "" || req.Secret == "" {
		return friendRequest{}, fmt.Errorf("invite missing fields")
	}
	return req, nil
}
