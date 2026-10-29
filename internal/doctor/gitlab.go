package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// GitLab access levels.
const (
	levelReporter   = 20
	levelMaintainer = 40
)

// GitLabInput is what the GitLab checks need.
type GitLabInput struct {
	URL          string
	Groups       []string
	BotToken     string
	ServiceToken string
	// Events: webhook, poll or both (gitlab.events).
	Events        string
	WebhookURL    string
	WebhookSecret string
	// CAFile: gitlab.ca_file (PEM added to the system roots, like the bot's
	// GitLab client); used when HTTP is nil.
	CAFile string
	// HTTP is the client (nil = 10 s timeout); Now the clock (nil = now).
	HTTP *http.Client
	Now  func() time.Time
}

// httpClient is the default client: 10 s timeout, CAFile on top of the
// system roots.
func httpClient(caFile string) (*http.Client, error) {
	hc := &http.Client{Timeout: 10 * time.Second}
	if caFile == "" {
		return hc, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("в файле нет PEM-сертификатов")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	hc.Transport = tr
	return hc, nil
}

// errUnauthorized: GitLab answered 401.
var errUnauthorized = errors.New("401")

// api is a read-only GitLab API client (GET only, by construction).
type api struct {
	base string
	http *http.Client
}

func (a api) get(ctx context.Context, token, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/api/v4"+path, nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("PRIVATE-TOKEN", token)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return resp.StatusCode, errUnauthorized
	case resp.StatusCode != http.StatusOK:
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

type gitlabUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// GitLab checks the GitLab side.
func GitLab(in GitLabInput) Provider {
	return ProviderFunc(func(ctx context.Context) []Result {
		if in.HTTP == nil {
			hc, err := httpClient(in.CAFile)
			if err != nil {
				return []Result{{ID: "gitlab.version", Group: "GitLab", Title: "адрес", Status: Fail,
					Detail: "gitlab.ca_file " + in.CAFile + " не прочитан: " + oneLine(err.Error()), Action: "проверьте путь и PEM-файл внутреннего CA"}}
			}
			in.HTTP = hc
		}
		if in.Now == nil {
			in.Now = time.Now
		}
		a := api{base: strings.TrimRight(in.URL, "/"), http: in.HTTP}
		var out []Result
		ver := Result{ID: "gitlab.version", Group: "GitLab", Title: "адрес"}
		reachable := false
		if u, err := url.Parse(a.base); in.URL == "" || err != nil || u.Host == "" {
			ver.Status, ver.Detail, ver.Action = Fail, "адрес GitLab не задан или неверный", "задайте gitlab.url (https://gitlab.example.com)"
		} else {
			var v struct {
				Version string `json:"version"`
			}
			_, err := a.get(ctx, in.BotToken, "/version", &v)
			switch {
			case err == nil:
				ver.Status, ver.Detail, reachable = OK, a.base+", GitLab "+v.Version, true
			case errors.Is(err, errUnauthorized) || strings.HasPrefix(err.Error(), "HTTP "):
				ver.Status, ver.Detail, reachable = OK, a.base+" отвечает", true
			default:
				ver.Status, ver.Detail = Fail, "GitLab недоступен: "+oneLine(err.Error())
				ver.Action = "проверьте адрес, DNS, TLS-сертификат (ca) и прокси (HTTPS_PROXY) на машине бота"
			}
		}
		out = append(out, ver)
		bot := Result{ID: "gitlab.bot_token", Group: "GitLab", Title: "токен бота"}
		var botUser *gitlabUser
		switch {
		case !reachable:
			bot.Status, bot.Detail = Skip, "GitLab недоступен"
		case in.BotToken == "":
			bot.Status, bot.Detail, bot.Action = Fail, "GITLAB_TOKEN не задан", "создайте токен бот-аккаунта со scope api и запишите в .env (`reviewbot init`)"
		default:
			botUser = tokenCheck(ctx, a, in, in.BotToken, &bot)
		}
		out = append(out, bot)
		if botUser != nil {
			for _, g := range in.Groups {
				out = append(out, roleCheck(ctx, a, in.BotToken, botUser.ID, g, "gitlab.bot_role."+g, "роль бота в "+g, false))
			}
		}
		svc := Result{ID: "gitlab.service_token", Group: "GitLab", Title: "служебный токен"}
		var svcUser *gitlabUser
		switch {
		case !reachable:
			svc.Status, svc.Detail = Skip, "GitLab недоступен"
		case in.ServiceToken == "":
			svc.Status, svc.Detail = Warn, "не задан: бот не закрывает треды, не назначает себя ревьюером, не ставит метки и не создаёт вебхуки"
			svc.Action = "необязательно: токен Maintainer в GITLAB_SERVICE_TOKEN включит эти функции"
		default:
			svcUser = tokenCheck(ctx, a, in, in.ServiceToken, &svc)
		}
		out = append(out, svc)
		if svcUser != nil {
			for _, g := range in.Groups {
				out = append(out, roleCheck(ctx, a, in.ServiceToken, svcUser.ID, g, "gitlab.service_role."+g, "роль служебного токена в "+g, true))
			}
		}
		return append(out, eventsCheck(in))
	})
}

// tokenCheck reads the token's user and scopes into r; nil when the token
// is not usable.
func tokenCheck(ctx context.Context, a api, in GitLabInput, token string, r *Result) *gitlabUser {
	var u gitlabUser
	if _, err := a.get(ctx, token, "/user", &u); err != nil {
		r.Status = Fail
		if errors.Is(err, errUnauthorized) {
			r.Detail, r.Action = "токен не принят (неверный, отозван или истёк)", "создайте новый токен со scope api и обновите .env"
		} else {
			r.Detail, r.Action = "не удалось прочитать пользователя: "+oneLine(err.Error()), "проверьте токен и доступ к GitLab API"
		}
		return nil
	}
	r.Status, r.Detail = OK, "пользователь "+u.Username
	var pat struct {
		Scopes    []string `json:"scopes"`
		ExpiresAt string   `json:"expires_at"`
	}
	if _, err := a.get(ctx, token, "/personal_access_tokens/self", &pat); err != nil {
		r.Detail += ", scope не проверен (GitLab старше 16.0 или не личный токен)"
		return &u
	}
	if !slices.Contains(pat.Scopes, "api") {
		r.Status, r.Detail = Fail, "пользователь "+u.Username+", нет scope api ("+strings.Join(pat.Scopes, ", ")+")"
		r.Action = "создайте токен со scope api: read_api не даёт писать комментарии"
		return &u
	}
	r.Detail += ", scope api"
	if exp, err := time.Parse("2006-01-02", pat.ExpiresAt); err == nil {
		if days := int(exp.Sub(in.Now()).Hours() / 24); days < 14 {
			r.Status, r.Detail = Warn, r.Detail+", истекает через "+strconv.Itoa(max(days, 0))+" дн."
			r.Action = "продлите токен или создайте новый до " + pat.ExpiresAt
		}
	}
	return &u
}

// roleCheck reads the user's role in a group: the bot must be Reporter
// (above that it could merge, below it cannot comment), the service token
// Maintainer or above.
func roleCheck(ctx context.Context, a api, token string, userID int64, group, id, title string, service bool) Result {
	r := Result{ID: id, Group: "GitLab", Title: title}
	var g struct {
		ID int64 `json:"id"`
	}
	if _, err := a.get(ctx, token, "/groups/"+url.PathEscape(group), &g); err != nil {
		r.Status, r.Detail = Fail, "группа "+group+" не найдена или недоступна токену"
		r.Action = "проверьте gitlab.groups и добавьте пользователя токена в группу"
		return r
	}
	var m struct {
		AccessLevel int `json:"access_level"`
	}
	if _, err := a.get(ctx, token, "/groups/"+strconv.FormatInt(g.ID, 10)+"/members/all/"+strconv.FormatInt(userID, 10), &m); err != nil {
		r.Status, r.Detail = Fail, "пользователь не участник группы "+group
		r.Action = "добавьте пользователя токена в группу " + group
		if !service {
			r.Action += " с ролью Reporter"
		} else {
			r.Action += " с ролью Maintainer"
		}
		return r
	}
	name := roleName(m.AccessLevel)
	switch {
	case service && m.AccessLevel >= levelMaintainer:
		r.Status, r.Detail = OK, name
	case service:
		r.Status, r.Detail, r.Action = Warn, name+": часть служебных функций не сработает", "дайте пользователю роль Maintainer в "+group
	case m.AccessLevel == levelReporter:
		r.Status, r.Detail = OK, name+" (нужна ровно эта)"
	case m.AccessLevel > levelReporter:
		r.Status, r.Detail, r.Action = Warn, name+": роль выше Reporter — бот сможет мержить и менять код", "понизьте бот-аккаунт до Reporter в "+group
	default:
		r.Status, r.Detail, r.Action = Fail, name+": бот не сможет писать комментарии", "дайте бот-аккаунту роль Reporter в "+group
	}
	return r
}

func roleName(level int) string {
	switch {
	case level >= 50:
		return "Owner"
	case level >= 40:
		return "Maintainer"
	case level >= 30:
		return "Developer"
	case level >= 20:
		return "Reporter"
	case level >= 10:
		return "Guest"
	}
	return "нет доступа"
}

// eventsCheck: how GitLab tells the bot about merge requests.
func eventsCheck(in GitLabInput) Result {
	r := Result{ID: "gitlab.events", Group: "GitLab", Title: "события MR"}
	switch in.Events {
	case "poll":
		r.Status, r.Detail = OK, "опрос API (вебхуки не нужны)"
	case "webhook", "both", "":
		switch {
		case in.WebhookSecret == "":
			r.Status, r.Detail = Fail, "вебхуки включены, но GITLAB_WEBHOOK_SECRET не задан"
			r.Action = "задайте секрет (`reviewbot init` сгенерирует) или gitlab.events: poll"
		case in.WebhookURL == "":
			r.Status, r.Detail = Warn, "секрет задан; адрес вебхука (gitlab.webhook_url) не задан — вебхуки настраиваются в GitLab вручную"
			r.Action = "задайте gitlab.webhook_url, чтобы `reviewbot sync-hooks` создал вебхуки сам"
		default:
			r.Status, r.Detail = OK, "вебхук "+in.WebhookURL+"; доставку проверит GitLab (Settings → Webhooks → Test)"
		}
	default:
		r.Status, r.Detail, r.Action = Fail, "неизвестный режим "+in.Events, "gitlab.events: webhook, poll или both"
	}
	return r
}
