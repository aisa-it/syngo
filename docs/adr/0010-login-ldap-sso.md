# ADR-0010: вход через LDAP, пароли Synapse (pepper, NFKC), SSO (OIDC)

Дата: 20.11.2026, OIDC - 21.11.2026. Статус: принято.

## Контекст

Типичная целевая инсталляция - замена существующего Synapse со входом через LDAP по нескольким
доменам (например, `example.org`, `example.com`, `example.net`: три модуля
`ldap_auth_provider.LdapAuthProviderModule` synapse-ldap3). Исходные проблемы:

- пользователи с `password_hash IS NULL` (LDAP, SSO, appservice) переносились с пустым хешем,
  провайдеров LDAP и SSO не было - войти после переезда они не могли;
- пустой хеш давал 500 на `/login` и этим раскрывал существование учетной записи;
- `password_config.pepper` и NFKC-нормализация Synapse не поддержаны - пароли с pepper или с
  не-NFKC символами не подходили; поиск пользователя - только точное совпадение `lower(localpart)`.

## Решение

### Пароли Synapse

Проверка bcrypt повторяет `synapse/handlers/auth.py` (`hash`, `validate_hash`): пароль
нормализуется NFKC, к нему дописывается `auth.password_pepper` (то же значение, что
`password_config.pepper` в homeserver.yaml), результат обрезается до 72 байт. После успешного
входа хеш прозрачно переписывается в argon2id (как раньше). Новые argon2id-хеши тоже считаются
от NFKC-формы; хеши, созданные до этого, проверяются и по исходной форме. Тесты - на векторах,
посчитанных кодом Synapse 1.165 (`internal/auth/auth_test.go`).

Пустой хеш - отказ `403 M_FORBIDDEN` с тем же телом, что у неверного пароля и неизвестного
пользователя. Поиск пользователя регистронезависим, как `_find_user_id_and_pwd_hash` Synapse:
одно совпадение - оно, несколько - только точное (индекс `lower(localpart)`, миграция 0035).

### LDAP

`core.PasswordProvider` - внешняя проверка пароля. Порядок как в Synapse: провайдеры по
порядку до первого успеха, затем локальный хеш (`auth.password_localdb_enabled`, по умолчанию
true). Ошибка провайдера (сервер недоступен) - в журнал, опрашивается следующий; неверный
пароль или нет пользователя - отказ без ошибки.

`internal/ldapauth` (go-ldap) совместим с synapse-ldap3: `auth.ldap` - список провайдеров с
полями модуля (`uri` строкой или списком, `start_tls`, `base`, `attributes.uid/mail/name`,
`bind_dn`, `bind_password` или `bind_password_file`, `filter`). Режимы:

| Режим | Когда | Как |
| --- | --- | --- |
| simple | `bind_dn` не задан | bind `<uid>=<имя>,<base>` паролем пользователя |
| search | `bind_dn` задан | bind служебной учеткой, поиск `(&(<uid>=<имя>)<filter>)` в `base`, ровно одна запись, bind ее DN паролем пользователя |

Имя экранируется (`EscapeFilter`, `EscapeDN`), пустой пароль отклоняется до обращения к
серверу (анонимный bind). Первый вход создает учетную запись: localpart - имя в нижнем
регистре, без локального пароля, displayname - атрибут `name` (как synapse-ldap3). В отличие от
synapse-ldap3, displayname обновляется при каждом входе и рассылается в комнаты
(`sync_displayname`, по умолчанию true). Пароль каталога подтверждает и UIA (удаление
устройств, сброс кросс-подписи). Три домена исходной инсталляции - три элемента `auth.ldap`.

```yaml
auth:
  password_pepper: "<password_config.pepper из homeserver.yaml>"
  ldap:
    - name: example.org
      uri: "ldap://ldap.example.org:389"
      start_tls: true
      base: "ou=users,dc=example,dc=org"
      attributes: {uid: "uid", mail: "mail", name: "cn"}
      bind_dn: "cn=matrix,ou=services,dc=example,dc=org"
      bind_password_file: /run/secrets/ldap-example
      filter: "(objectClass=posixAccount)"
```

### SSO (OIDC)

Сначала была только точка расширения (учетка SSO - учетка без пароля). Затем реализован
вход через OpenID Connect, совместимый с Synapse настолько, чтобы при переезде
не перенастраивать IdP и клиентов:

| Что | Как |
| --- | --- |
| Конфиг | `auth.oidc_providers` - поля `oidc_providers` Synapse (`idp_id`, `idp_name`, `idp_icon`, `idp_brand`, `issuer`, `discover`, `client_id`, `client_secret`/`client_secret_path`, `client_auth_method`, `scopes`, `pkce_method`, `user_profile_method`, `allow_existing_users`, `attribute_requirements`, `user_mapping_provider.config`); `auth.sso` - `client_whitelist`, `update_profile_information`; `server.public_baseurl` |
| Идентификатор | `idp_id` с префиксом `oidc-` (кроме `oidc`), как в Synapse: это `auth_provider` в `user_external_ids` и `idpId` клиента |
| Секрет клиента | `SYNGOD_OIDC_CLIENT_SECRET_<IDP_ID>`, затем `client_secret_path`, затем `client_secret` |
| Эндпоинты | `GET /login` - `m.login.sso` с `identity_providers` и `m.login.token` (Element: кнопка «Continue with ...»); `GET /login/sso/redirect[/{idpId}]?redirectUrl=` (v3 и r0); callback `/_synapse/client/oidc/callback` (адрес Synapse); `POST /login` `m.login.token` |
| Протокол | `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2`: discovery при первом входе (кешируется), authorization code, PKCE S256 (`auto` - если IdP объявил), проверка `id_token`: подпись по JWKS, `iss`, `aud`, `exp`; `nonce` сверяется с сессией |
| Сессия браузера | cookie `syngo_oidc_session` (HttpOnly, SameSite=Lax, Secure при https, путь `/_synapse/client/oidc`, 15 минут): `state`, `nonce`, PKCE verifier, `redirectUrl` в AES-256-GCM, ключ случайный на процесс. Без cookie или с чужим `state` callback отказывает до обмена `code` |
| Маппинг | `localpart_template`, `display_name_template` - подмножество Jinja (`{{ user.claim }}`, вложенные claims, фильтры `lower`, `upper`, `trim`, `localpart_from_email`), остальное - ошибка конфига при старте; localpart проходит `map_username_to_mxid_localpart` Synapse; занятое имя - суффикс 1, 2, ... (или привязка при `allow_existing_users`) |
| Учетки | `user_external_ids (auth_provider, external_id, user_id)` (миграция 0037): повторный вход - по `subject`, первый - создание учетки без пароля |
| loginToken | `syl_...`, 2 минуты, одноразовый: в `login_tokens` (миграция 0037) только SHA-256, погашение одним `UPDATE ... WHERE used_at IS NULL AND expires_at > now` |
| Адрес возврата | `redirectUrl` - абсолютный, без `javascript:`/`data:`/`vbscript:`/`file:`; из `sso.client_whitelist` (префикс по границе пути) - сразу 302 с `loginToken`, иначе страница подтверждения «Продолжить вход?», как в Synapse |
| UIA | учетке без пароля (не LDAP, пустой хеш) стадия `m.login.sso`, с паролем - `m.login.password` (и `m.login.sso`, если есть привязка). Fallback `/auth/m.login.sso/fallback/web?session=` уводит на провайдера; стадия засчитывается, только если провайдер подтвердил ту же учетку (`user_external_ids`), сессия UIA привязана к пользователю и действию (метод и путь) |
| Перенос | `export.sql` выгружает `user_external_ids` (kind `user_external_id`), `syngo-import` переносит, `-verify` сверяет `external_ids` |

Пример (Keycloak):

```yaml
server:
  public_baseurl: "https://matrix.example.org/"
auth:
  sso:
    client_whitelist: ["https://element.example.org/"]
  oidc_providers:
    - idp_id: keycloak
      idp_name: "Корпоративный вход"
      issuer: "https://sso.example.org/realms/main"
      client_id: matrix
      client_secret_path: /run/secrets/oidc-keycloak
      scopes: ["openid", "profile"]
      user_mapping_provider:
        config:
          localpart_template: "{{ user.preferred_username }}"
          display_name_template: "{{ user.name }}"
```

## Отложено

- SAML, CAS, JWT-вход; OIDC: `private_key_jwt`, `backchannel_logout`, `subject_template`,
  выбор имени пользователем (username picker - `localpart_template` обязателен), `email_template`
  (3PID нет), полный Jinja в шаблонах, хранение сессий UIA в БД (после перезапуска UIA m.login.sso
  начинается заново), MSC3861 (делегирование OIDC, Element X без SSO-редиректа).
- synapse-ldap3 `active_directory`, `default_domain`, вход по mail (`uid: mail` работает как обычный атрибут).
- Атрибут `mail` в 3PID: таблицы 3PID в SYNGO нет, адрес только в журнале.
- Блокировка `locked` Synapse (M_USER_LOCKED): при импорте такие пользователи деактивируются.

## Последствия

- Пользователи LDAP входят тем же логином и паролем, что в Synapse; сессии переносятся, повторный вход не нужен.
- Недоступность LDAP не мешает локальным учеткам (оператор, администратор).
- Новая зависимость - `github.com/go-ldap/ldap/v3`.
- Инсталляции с SSO переезжают без перенастройки IdP (тот же callback и `idp_id`),
  пользователи SSO входят в перенесенные учетки. Новые зависимости - `github.com/coreos/go-oidc/v3`
  v3.21.0, `golang.org/x/oauth2` v0.38.0 (и `github.com/go-jose/go-jose/v4` v4.1.4 через go-oidc).
