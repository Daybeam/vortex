package api

import (
	"context"
	"net/http"
)

const tgMaxMsgLen = 4096

type TelegramChannel struct{}

func NewTelegramChannel(h *ChatHandler, token string) *TelegramChannel {
	return &TelegramChannel{}
}

func (t *TelegramChannel) Name() string { return "telegram" }

func (t *TelegramChannel) Start(ctx context.Context) error {
	return nil
}

func (t *TelegramChannel) Stop() {}

func tgCall(token string, client *http.Client, method string, body []byte, out any) error {
	return nil
}

func tgRedact(token, s string) string { return s }

func IsAdminCommand(cmd string) bool { return false }
