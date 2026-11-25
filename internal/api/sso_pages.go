package api

import (
	"bytes"
	"html/template"
	"net/http"
)

// Страницы браузера при входе через SSO: ошибки, выбор провайдера,
// подтверждение адреса возврата вне sso.client_whitelist и стадия UIA m.login.sso.
// Все значения экранирует html/template; страницы нельзя встроить во фрейм.

// Тексты страниц.
const (
	msgInternal            = "Внутренняя ошибка сервера. Попробуйте войти еще раз."
	msgNotConfigured       = "Вход через SSO на этом сервере не настроен."
	msgProviderUnavailable = "Провайдер входа недоступен. Попробуйте позже."
	msgProviderRefused     = "Провайдер входа отказал во входе."
	msgSessionMissing      = "Сессия входа не найдена или истекла (cookie отключены?). Начните вход заново из приложения."
	msgSessionInvalid      = "Сессия входа не подходит к ответу провайдера. Начните вход заново из приложения."
	msgInvalidResponse     = "Ответ провайдера входа не прошел проверку."
	msgNotAllowed          = "Учетной записи провайдера не разрешен вход на этот сервер."
	msgMapping             = "Не удалось сопоставить учетную запись провайдера с учетной записью Matrix."
	msgDeactivated         = "Учетная запись Matrix деактивирована."
	msgUIASessionUnknown   = "Сессия подтверждения не найдена или истекла. Повторите действие в приложении."
	msgNoLinkedProvider    = "Учетная запись не привязана к провайдеру входа."
	msgIdentityMismatch    = "Вход выполнен другой учетной записью провайдера, чем требуется для подтверждения."
)

// ssoLink - ссылка на странице (провайдер или адрес возврата).
type ssoLink struct {
	Name string
	URL  string
}

// ssoPageData - данные шаблонов.
type ssoPageData struct {
	Message string
	Detail  string
	Target  string
	// Link - адрес возврата клиента с loginToken; схема уже проверена
	// (sso.CheckRedirectURL), template.URL пропускает схемы приложений (element://).
	Link  template.URL
	Links []ssoLink
}

const ssoPageHead = `<!DOCTYPE html>
<html lang="ru"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{template "title" .}}</title>
<style>body{font-family:sans-serif;max-width:32em;margin:3em auto;padding:0 1em;line-height:1.5}
a.button{display:block;margin:.5em 0;padding:.6em 1em;border:1px solid #0dbd8b;border-radius:6px;
color:#0dbd8b;text-decoration:none;text-align:center}code{word-break:break-all}</style>
</head><body>`

const ssoPageFoot = `</body></html>`

var (
	pageError = template.Must(template.New("error").Parse(ssoPageHead +
		`<h1>Не удалось войти</h1><p>{{.Message}}</p>{{if .Detail}}<p><code>{{.Detail}}</code></p>{{end}}` +
		ssoPageFoot + `{{define "title"}}Ошибка входа{{end}}`))

	pagePicker = template.Must(template.New("picker").Parse(ssoPageHead +
		`<h1>Выберите способ входа</h1>{{range .Links}}<a class="button" href="{{.URL}}">{{.Name}}</a>{{end}}` +
		ssoPageFoot + `{{define "title"}}Вход{{end}}`))

	pageConfirmRedirect = template.Must(template.New("confirm").Parse(ssoPageHead +
		`<h1>Продолжить вход?</h1><p>Приложение по адресу <code>{{.Target}}</code> получит доступ к вашей
учетной записи Matrix. Продолжайте, только если вход начали вы и адрес вам знаком.</p>
<a class="button" href="{{.Link}}">Продолжить</a>` +
		ssoPageFoot + `{{define "title"}}Подтверждение входа{{end}}`))

	pageUIAConfirm = template.Must(template.New("uia").Parse(ssoPageHead +
		`<h1>Подтвердите действие</h1><p>Для действия <code>{{.Target}}</code> подтвердите, что это вы,
входом через провайдера.</p>{{range .Links}}<a class="button" href="{{.URL}}">{{.Name}}</a>{{end}}` +
		ssoPageFoot + `{{define "title"}}Подтверждение{{end}}`))

	// pageUIADone - завершение fallback по спецификации: window.onAuthDone или
	// postMessage("authDone") окну клиента.
	pageUIADone = template.Must(template.New("done").Parse(ssoPageHead +
		`<h1>Готово</h1><p>Личность подтверждена. Вернитесь в приложение.</p>
<script>if (window.onAuthDone) { window.onAuthDone(); } else if (window.opener && window.opener.postMessage) { window.opener.postMessage("authDone", "*"); }</script>` +
		ssoPageFoot + `{{define "title"}}Подтверждено{{end}}`))
)

// writeSSOPage отдает HTML-страницу.
func writeSSOPage(w http.ResponseWriter, status int, page *template.Template, data ssoPageData) {
	var buf bytes.Buffer
	if err := page.Execute(&buf, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; frame-ancestors 'none'; form-action 'none'")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
