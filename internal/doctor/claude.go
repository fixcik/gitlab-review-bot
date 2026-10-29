package doctor

import (
	"context"
	"strings"
)

// ClaudeInput: the Claude credentials (CLAUDE_CODE_OAUTH_TOKEN or
// ANTHROPIC_API_KEY). Only their presence and format are checked; a live
// call costs money and happens on the first review.
type ClaudeInput struct {
	OAuthToken, APIKey string
}

// Claude checks the Claude token.
func Claude(in ClaudeInput) Provider {
	return ProviderFunc(func(context.Context) []Result {
		r := Result{ID: "claude.token", Group: "Claude", Title: "токен"}
		switch {
		case in.OAuthToken != "" && strings.HasPrefix(in.OAuthToken, "sk-ant-oat"):
			r.Status, r.Detail = OK, "CLAUDE_CODE_OAUTH_TOKEN принят (формат); живая проверка — на первом ревью"
		case in.APIKey != "" && strings.HasPrefix(in.APIKey, "sk-ant-api"):
			r.Status, r.Detail = OK, "ANTHROPIC_API_KEY принят (формат); живая проверка — на первом ревью"
		case in.OAuthToken != "" || in.APIKey != "":
			r.Status, r.Detail = Warn, "токен задан, но формат необычный (ожидается sk-ant-oat… или sk-ant-api…)"
			r.Action = "проверьте, что скопирован весь токен из `claude setup-token` или консоли Anthropic"
		default:
			r.Status, r.Detail = Fail, "нет ни CLAUDE_CODE_OAUTH_TOKEN, ни ANTHROPIC_API_KEY"
			r.Action = "получите токен `claude setup-token` (подписка) или ключ API и запишите в .env (`reviewbot init`)"
		}
		return []Result{r}
	})
}
