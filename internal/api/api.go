// Package api реализует HTTP-транспорт: роутер, middleware, обработчики
// Client-Server API и mapping доменных ошибок в коды Matrix/HTTP.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/aisa-it/syngo/internal/auth"
	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/join"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
	"github.com/aisa-it/syngo/internal/media"
	"github.com/aisa-it/syngo/internal/pushers"
	"github.com/aisa-it/syngo/internal/store"
)

// Server собирает HTTP-роутер поверх конфигурации и зависимостей.
type Server struct {
	// uiaFails — неудачные пароли в UIA по пользователям.
	uiaFails uiaFailures
	cfg      config.Config
	db       *store.DB
	auth     *core.Auth
	rooms    *core.RoomService
	keys     *core.KeysService
	// keyBackup и crossSigning - сервисы E2EE поверх хранилища.
	keyBackup    *core.KeyBackupService
	crossSigning *core.CrossSigningService
	// pushRules есть всегда (без БД - только серверные правила), pushers - с БД.
	pushRules      *core.PushRulesService
	pushersService *core.PushersService
	accountData    *core.AccountDataService
	admin          *core.AdminService
	profiles       core.ProfileStore
	// presenceAudience отбирает, чей presence показывать в /sync (общая комната).
	presenceAudience presenceAudience
	// transactionIDs - txn_id событий устройства для unsigned.transaction_id.
	transactionIDs transactionIDLookup
	// roomSummaries - summary комнат в /sync при lazy_load_members (store).
	roomSummaries roomSummaryLookup
	// deviceListLeft - device_lists.left в /sync (store).
	deviceListLeft deviceListLeftLookup
	// syncFilterCache - разобранные сохраненные фильтры /sync (userID+id).
	syncFilterCache sync.Map
	syncFilterCount atomic.Int64
	// toDevice - с БД, filters - всегда (без БД фильтр не сохраняется).
	toDevice *core.ToDeviceService
	// remoteKeys - ключи и устройства пользователей других серверов, с БД.
	remoteKeys *core.RemoteKeysService
	filters    *core.FilterService
	// memberships и profilePropagator - с БД.
	memberships       *core.MembershipService
	profilePropagator *core.ProfilePropagator
	// historyVisibility - с БД; readMarkers - всегда (без БД receipts только в памяти).
	historyVisibility *core.VisibilityService
	readMarkers       *core.ReadMarkersService
	// search и directory - с БД.
	search    *core.SearchService
	directory *core.DirectoryService
	// stateResolution и redactions - с БД.
	stateResolution *core.StateResolutionService
	redactions      *core.RedactionLookup
	// aggregations - bundled aggregations, с БД.
	aggregations *core.AggregationsService
	// health есть всегда: без БД readiness не готов, метрик пула нет.
	health     *core.HealthService
	ephemeral  *core.EphemeralStore
	syncLoader *core.SyncDataLoader
	// syncService - данные /sync сверх лоадера (to-device, ключи, state), с БД.
	syncService  *core.SyncService
	syncStore    syncPositionStore
	syncTokens   *core.SyncTokenCodec
	syncNotifier *syncNotifier
	// slidingSync - simplified sliding sync (MSC4186), с БД;
	// toDeviceAck удаляет to-device, подтвержденный закладкой расширения.
	slidingSync *core.SlidingSyncService
	toDeviceAck toDeviceAcker
	// pushActions - хранимые действия push-правил и счетчики непрочитанного,
	// notifications - /notifications; оба с БД.
	pushActions   *core.PushActionsService
	notifications *core.NotificationsService
	// pushUserLocks сериализует досчет счетчиков с receipts пользователя.
	pushUserLocks keyedLock
	// pushSyncWait - ожидание фонового разбора push-действий в /sync (0 - по
	// умолчанию, 200 мс; интеграционные тесты увеличивают).
	pushSyncWait time.Duration
	// federationTxnLocks - транзакции /send одного origin обрабатываются по
	// очереди (Synapse _transaction_linearizer).
	federationTxnLocks keyedLock
	// syncLocks сериализует выдачу to-device по устройству.
	syncLocks      sync.Map
	uia            *core.MemoryUIASessions
	registerNonces *nonceStore // nonce регистрации через общий секрет
	openid         *openIDStore
	log            *slog.Logger
	startedAt      time.Time
	limiters       limiters
	clientIPs      clientIPResolver // IP клиента с учетом server.trusted_proxies
	media          *media.Service
	federationKey  *signing.Key
	// keyFetcher - получение ключей чужих серверов (нотарии, затем сам сервер через
	// резолвер и защищенный транспорт); кэш - в БД.
	keyFetcher *transport.KeyFetcher
	// keyLookup подменяется в тестах (в проде — lookupServerKey с кэшем БД).
	keyLookup func(origin, keyID string) (string, int64, error)
	// federationHosts - allow_hosts/deny_hosts и для входящих запросов.
	federationHosts transport.HostPolicy
	// federationClient и incoming — транспорт федерации, подключается в main.
	federationTransport *transport.Client
	federationIncoming  *transport.Incoming
	remoteJoiner        *join.Joiner
	// remoteQueries - кэш /query/profile и /query/directory чужих серверов.
	remoteQueries remoteQueryCache
	// hierarchySessions - next_batch клиентского /hierarchy.
	hierarchySessions hierarchySessions
	// outboxStats — статистика очереди федерации для /metrics и /health/ready.
	outboxStats func(context.Context) (store.OutboxStats, error)
	// draining — сервер завершает работу: long-poll /sync не ждёт.
	draining atomic.Bool
	// syncDuration - обработка /sync без ожидания long-poll;
	// httpRequests/httpDuration - запросы публичного листенера по маршрутам.
	syncDuration *metricVec
	httpRequests *metricVec
	httpDuration *metricVec
	syncCount    atomic.Int64
	rateLimited  atomic.Int64
	// profileLocks — сериализация рассылки профиля по пользователю:
	// keyed-lock с подсчётом ссылок, записи удаляются после последнего ожидающего.
	profileLocks keyedLock
	router       http.Handler
	opsRouter    http.Handler
	// schemaVersion - ожидаемая версия схемы для readiness.
	schemaVersion uint
	// instanceLockHeld - удерживается ли блокировка экземпляра;
	// nil - блокировки нет (тесты, запуск без БД).
	instanceLockHeld func() bool

	// Доставка push: общий диспетчер создается при первом push.
	pushOnce    sync.Once
	pushQueue   atomic.Pointer[pushers.Dispatcher]
	pushOptions pushers.DeliveryOptions
	// pushResolver и pushUnsafeAddr подменяются в тестах SSRF-защиты.
	pushResolver   pushResolver
	pushUnsafeAddr func(netip.Addr) bool

	// sso - вход через OIDC; nil - не настроен.
	sso *ssoServer
}

// newFederationHTTPClient — HTTP-клиент для выборки ключей пиров. На стенде
// с self-signed CA учитывает federation.insecure_skip_verify: без этого
// входящие федеративные запросы падали 401 «certificate signed by unknown authority».
// newKeyFetcher - получение ключей чужих серверов по конфигу федерации: тот же
// резолвер, блок-лист адресов и allow/deny, что у исходящего транспорта.
func newKeyFetcher(cfg config.Config) *transport.KeyFetcher {
	policy, err := transport.NewAddressPolicy(cfg.Federation.IPRangeBlacklist, cfg.Federation.IPRangeWhitelist)
	if err != nil {
		// Конфиг проверен Validate; на всякий случай - строгая политика по умолчанию.
		policy = transport.DefaultAddressPolicy()
	}

	client := transport.NewClient(transport.Options{
		Scheme:             cfg.Federation.Scheme,
		InsecureSkipVerify: cfg.Federation.InsecureSkipVerify,
		AddressPolicy:      policy,
		AllowHosts:         cfg.Federation.AllowHosts,
		DenyHosts:          cfg.Federation.DenyHosts,
		MaxResponseBytes:   1 << 20,
		ConnectTimeout:     cfg.Federation.ConnectTimeout.Std(),
		RequestTimeout:     10 * time.Second,
	})

	notaries := make([]transport.TrustedKeyServer, 0, len(cfg.Federation.TrustedKeyServers))
	for _, notary := range cfg.Federation.TrustedKeyServers {
		notaries = append(notaries, transport.TrustedKeyServer{ServerName: notary.ServerName, VerifyKeys: notary.VerifyKeys})
	}

	return &transport.KeyFetcher{Client: client, TrustedKeyServers: notaries}
}

// corsMiddleware разрешает запросы с любых origin. Matrix-клиенты (Element Web)
// ходят с отдельного origin; API аутентифицируется Bearer-токеном, а не cookie,
// поэтому Access-Control-Allow-Origin: * безопасен.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept, Authorization")
		h.Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// New создаёт сервер и собирает роутер. db может быть nil — тогда сервис
// работает без базы (readiness сообщит об этом).
func New(cfg config.Config, db *store.DB, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}

	s := &Server{
		cfg:             cfg,
		db:              db,
		log:             log,
		startedAt:       time.Now(),
		limiters:        newLimiters(cfg.RateLimit),
		keyFetcher:      newKeyFetcher(cfg),
		federationHosts: transport.HostPolicy{Allow: cfg.Federation.AllowHosts, Deny: cfg.Federation.DenyHosts},
		syncDuration: newHistogramVec("syngo_sync_duration_seconds",
			"Обработка /sync без ожидания long-poll.", latencyBuckets),
		httpRequests: newCounterVec("syngo_http_requests_total",
			"Запросы публичного листенера по методу, шаблону маршрута и коду ответа.", "method", "route", "code"),
		httpDuration: newHistogramVec("syngo_http_request_duration_seconds",
			"Длительность запросов публичного листенера (для /sync включает long-poll).",
			latencyBuckets, "method", "route"),
	}

	if db != nil {
		s.auth = core.NewAuth(
			store.NewAuthStore(db),
			auth.NewMultiHasher().WithBcryptPepper(cfg.Auth.PasswordPepper),
			auth.NewTokenSource(),
			core.AuthOptions{
				PasswordProviders:     s.passwordProviders(cfg),
				DisableLocalPasswords: !cfg.Auth.LocalPasswordsEnabled(),
				OnProviderError: func(provider string, err error) {
					s.log.Warn("auth: password provider failed", "provider", provider, "error", err)
				},
				OnExternalLogin:   s.syncExternalProfile,
				ServerName:        cfg.Matrix.ServerName,
				RegistrationOpen:  cfg.Auth.RegistrationOpen,
				MinPasswordLength: cfg.Auth.MinPasswordLength,
				RefreshTokens:     cfg.Auth.RefreshTokens,
				AccessTokenTTL:    cfg.Auth.AccessTokenTTL.Std(),
				RefreshTokenTTL:   cfg.Auth.RefreshTokenTTL.Std(),
				// Прозрачная миграция паролей: bcrypt (Synapse) → argon2id при входе.
				OnPasswordRehash: func(userID string, err error) {
					if err != nil {
						s.log.Warn("auth: password rehash failed", "user_id", userID, "error", err)
						return
					}

					s.log.Info("auth: password rehashed to argon2id", "user_id", userID)
				},
			},
		)

		roomStore := store.NewRoomStore(db)
		profileStore := store.NewProfileStore(db)
		// Профиль участника - в m.room.member при join и invite.
		s.rooms = core.NewRoomService(roomStore, cfg.Matrix.ServerName).WithProfiles(profileStore.Profile)
		s.admin = core.NewAdminService(db, roomStore, s.rooms, core.AdminOptions{
			ServerName:     cfg.Matrix.ServerName,
			AuditRetention: cfg.Admin.AuditRetention.Std(),
			Warn: func(ctx context.Context, msg string, err error, args ...any) {
				s.log.WarnContext(ctx, "api: "+msg, append(args, "error", err)...)
			},
		})
		syncStore := store.NewSyncStore(db)
		s.syncStore = syncStore
		s.roomSummaries = syncStore
		s.syncLoader = core.NewSyncDataLoader(syncStore)
		s.memberships = core.NewMembershipService(syncStore)
		s.historyVisibility = core.NewVisibilityService(db, func(ctx context.Context, err error) {
			s.log.WarnContext(ctx, "api: history visibility failed", "error", err)
		})
		s.search = core.NewSearchService(db, syncStore, s.historyVisibility)
		s.directory = core.NewDirectoryService(roomStore)
		s.redactions = core.NewRedactionLookup(db)
		s.transactionIDs = db
		s.deviceListLeft = db
		s.aggregations = core.NewAggregationsService(db, func(ctx context.Context, msg string, err error) {
			s.log.WarnContext(ctx, "api: "+msg, "error", err)
		})
		s.stateResolution = core.NewStateResolutionService(db, func(ctx context.Context, roomID string, changed, conflicted int) {
			s.log.InfoContext(ctx, "federation: state resolution applied",
				"room_id", roomID, "changed_keys", changed, "conflicted", conflicted)
		})

		// Медиа: метаданные в БД, байты — в локальном объектном хранилище.
		if cfg.Media.StorageBackend == "local" {
			objects, err := media.NewLocalStore(cfg.Media.Directory)
			if err != nil {
				s.log.Error("media: local object store is unavailable", "error", err, "dir", cfg.Media.Directory)
			} else {
				s.media = media.NewService(store.NewMediaStore(db), objects,
					cfg.Matrix.ServerName, cfg.Media.MaxUploadBytes)
				s.media.Configure(mediaOptions(cfg.Media))

				// Медиа других серверов - по федерации.
				if cfg.Federation.Enabled {
					s.media.SetRemoteFetcher(federationMediaFetcher{s: s})
				}
			}
		}
		keysStore := store.NewKeysStore(db)
		s.keys = core.NewKeysService(keysStore, cfg.Matrix.ServerName).
			WithDeviceLists(db, s.logRecordKeysChange)
		s.syncService = core.NewSyncService(syncStore, db, keysStore)
		s.slidingSync = core.NewSlidingSyncService(syncStore, syncStore, syncStore, db, s.historyVisibility)
		s.toDeviceAck = db
		s.keyBackup = core.NewKeyBackupService(db)
		s.pushersService = core.NewPushersService(db, s.checkPushURL)
		s.crossSigning = core.NewCrossSigningService(db, s.keys, s.logRecordKeysChange)
		s.profiles = profileStore
		s.presenceAudience = profileStore
		s.toDevice = core.NewToDeviceService(db)
		if cfg.Federation.Enabled {
			s.toDevice.WithFederation(cfg.Matrix.ServerName, db, newFederationMessageID)
		}
		s.remoteKeys = core.NewRemoteKeysService(db, federationKeysClient{s: s}, cfg.Matrix.ServerName,
			func(context.Context, string) { s.notifySync() },
			func(ctx context.Context, msg string, err error, args ...any) {
				s.log.WarnContext(ctx, "api: "+msg, append(args, "error", err)...)
			})
		s.profilePropagator = core.NewProfilePropagator(s.profiles, syncStore, s.rooms, core.ProfilePropagationOptions{
			Lock: s.profileLocks.Lock,
			Warn: func(ctx context.Context, msg string, err error, args ...any) {
				s.log.WarnContext(ctx, "api: "+msg, append(args, "error", err)...)
			},
			Skip: func(ctx context.Context, msg string, err error, args ...any) {
				s.log.DebugContext(ctx, "api: "+msg, append(args, "error", err)...)
			},
		})
	}

	// Учетки SSO и loginToken - в БД; без нее вход через SSO недоступен.
	var ssoLinks core.SSOStore
	if db != nil {
		ssoLinks = store.NewSSOStore(db)
	}

	s.sso = s.newSSO(cfg, ssoLinks)

	// Фильтры без БД получают id, но не сохраняются; nil *store.DB в интерфейс не
	// передаем (см. push-правила ниже).
	var filterStore core.FilterStore
	if db != nil {
		filterStore = db
	}

	s.filters = core.NewFilterService(filterStore)

	var healthStore core.HealthStore
	if db != nil {
		healthStore = db
	}

	s.health = core.NewHealthService(healthStore)

	// Push-правила без БД отдают серверные правила; nil *store.DB в интерфейс не
	// передаем - иначе получится непустой интерфейс с nil внутри.
	var pushRulesStore core.PushRulesStore
	if db != nil {
		pushRulesStore = db
	}

	s.pushRules = core.NewPushRulesService(pushRulesStore, s.pushRulesHooks())
	if db != nil {
		s.accountData = core.NewAccountDataService(db, s.pushRules)
	}

	// Кодек токенов /sync нужен и без БД (тесты маршрутов): сам по себе он БД не требует.
	s.syncTokens = core.NewSyncTokenCodec(cfg.Matrix.ServerName, syncTokenSecret(cfg.Sync.TokenSecret))
	if cfg.Sync.TokenSecret == "" {
		// Конфиг пропускает это только для dev/test, но предупреждаем явно:
		// иначе токены молча умрут после рестарта.
		s.log.Warn("sync: token secret is not configured, using a random one for this process "+
			"(/sync tokens won't survive a restart)", "env", cfg.Env)
	}

	// Сессии UIA и нотифаер /sync живут в памяти процесса.
	s.uia = core.NewMemoryUIASessions()
	s.registerNonces = newNonceStore()
	s.openid = newOpenIDStore()
	s.syncNotifier = newSyncNotifier()
	s.ephemeral = core.NewEphemeralStore()
	if db != nil {
		s.pushActions = s.newPushActions(db)
		s.notifications = core.NewNotificationsService(db, s.pushActions.ReadStates())
	}

	// Отметки прочтения из БД: после рестарта клиенты видят их как прежде.
	var markerStore core.ReadMarkerStore
	if db != nil {
		markerStore = db
	}

	s.readMarkers = core.NewReadMarkersService(s.ephemeral, markerStore, func(ctx context.Context, err error) {
		s.log.WarnContext(ctx, "api: save receipt", "error", err)
	})
	// Счетчики непрочитанного сбрасываются до появления receipt в потоке /sync.
	if db != nil {
		s.readMarkers.OnReceiptStored(func(ctx context.Context, receipt core.ReceiptEvent) {
			if err := s.pushActions.ApplyReceipt(ctx, receipt); err != nil {
				s.log.WarnContext(ctx, "api: reset unread counts", "error", err)
			}
		})
	}

	if err := s.readMarkers.Load(context.Background()); err != nil {
		s.log.Warn("api: load receipts", "error", err)
	}

	// Разбор отставания и досчет старых событий - в фоне при старте, а не в
	// первых запросах. Только после загрузки receipts: досчет берет
	// отметки из памяти процесса, и раньше на большой базе он успевал начаться до
	// загрузки - история считалась непрочитанной от последнего своего сообщения.
	if s.pushActions != nil {
		s.pushActions.Signal()
	}

	// Конфиг уже провалидирован (Load); ошибка здесь - конфиг собран в обход
	// Validate, и заголовкам прокси тогда не доверяем вовсе.
	if trusted, err := cfg.Server.TrustedProxyPrefixes(); err != nil {
		s.log.Error("api: server.trusted_proxies is invalid, proxy headers are ignored", "error", err)
	} else {
		s.clientIPs = clientIPResolver{trusted: trusted}
	}

	r := chi.NewRouter()
	// middleware.RealIP намеренно не используется: в chi он помечен deprecated
	// и подменяет r.RemoteAddr по X-Forwarded-For от любого источника (спуфинг IP).
	// IP клиента определяет resolveClientIP - заголовки только от
	// server.trusted_proxies.
	r.Use(middleware.RequestID)
	r.Use(s.resolveClientIP)
	// Метрики снаружи recoverer: паника считается ответом 500.
	r.Use(s.instrument)
	r.Use(s.recoverer)
	r.Use(s.logRequests)
	// CORS нужен веб-клиентам (Element Web): без него браузер блокирует
	// запросы с другого origin и preflight OPTIONS.
	r.Use(corsMiddleware)

	r.NotFound(s.handleNotFound)
	r.MethodNotAllowed(s.handleMethodNotAllowed)

	// Служебные пробы: краткие liveness и readiness - для балансировщика и
	// healthcheck контейнера. /metrics - только на служебном листенере
	// (OpsHandler); на публичном - лишь в dev/test без ops_addr.
	r.Get("/health", s.handleHealth)
	r.Get("/health/ready", s.handleReady)

	if cfg.Server.OpsAddr == "" && cfg.IsDev() {
		r.Get("/metrics", s.handleMetrics)
	}

	// Admin-контур (ADR-0008): отдельный префикс, только с admin-токеном.
	// Admin-логин под тем же лимитом по IP, что и CS-логин: иначе пароль админа
	// можно брутфорсить мимо лимитера.
	r.With(s.rateLimit(limitLoginIP, ipKey)).Post("/_syngo/admin/v1/login", s.handleAdminLogin)

	// Регистрация оператором через общий секрет, совместимо с Synapse.
	// Возврат от провайдера OIDC - по адресу Synapse.
	r.Get("/_synapse/client/oidc/callback", s.handleOIDCCallback)

	r.Get("/_synapse/admin/v1/register", s.handleSharedSecretNonce)
	r.With(s.rateLimit(limitRegisterIP, ipKey)).Post("/_synapse/admin/v1/register", s.handleSharedSecretRegister)
	r.Group(func(r chi.Router) {
		r.Use(s.authenticate, s.requireAdmin)
		r.Get("/_syngo/admin/v1/users", s.handleAdminListUsers)
		r.Get("/_syngo/admin/v1/audit", s.handleAdminAudit)
		r.Get("/_syngo/admin/v1/rooms", s.handleAdminListRooms)
		r.Get("/_syngo/admin/v1/rooms/{roomId}/block", s.handleAdminRoomBlockStatus)
		// Выход доступен любому admin-токену, в том числе admin:read.
		r.Post("/_syngo/admin/v1/logout", s.handleAdminLogout)

		// Изменяющие операции — только scope admin.
		r.Group(func(r chi.Router) {
			r.Use(s.requireAdminWrite)
			r.Post("/_syngo/admin/v1/users/{userId}/{action}", s.handleAdminUserAction)
			r.Delete("/_syngo/admin/v1/rooms/{roomId}", s.handleAdminDeleteRoom)
			r.Put("/_syngo/admin/v1/rooms/{roomId}/block", s.handleAdminRoomBlock)
		})
	})

	// Каталог комнат: резолв без авторизации, изменение — по токену.
	r.Get("/_matrix/client/v3/directory/room/{alias}", s.handleDirectoryResolve)
	r.Get("/_matrix/client/v3/directory/list/room/{roomId}", s.handleRoomVisibilityGet)
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Put("/_matrix/client/v3/directory/room/{alias}", s.handleDirectoryCreate)
		r.Delete("/_matrix/client/v3/directory/room/{alias}", s.handleDirectoryDelete)
		r.Put("/_matrix/client/v3/directory/list/room/{roomId}", s.handleRoomVisibilityPut)
	})

	// Ограничения входящих федеративных запросов: тело и rate limit по origin.
	s.federationIncoming = transport.NewIncoming(
		cfg.Federation.IncomingRatePerSecond, cfg.Federation.IncomingBurst, cfg.Federation.MaxTransactionBytes)
	// До проверки подписи лимит - по IP клиента с учетом trusted_proxies.
	s.federationIncoming.ClientIP = clientIP

	// Федерация: публикация ключей сервера (остальные маршруты — следующими блоками).
	s.registerFederationRoutes(r)

	// Медиа-репозиторий: пути /_matrix/client/v1/media/* и легаси /_matrix/media/v3/*.
	s.registerMediaRoutes(r)

	// Matrix Client-Server API.
	r.Route("/_matrix/client", func(r chi.Router) {
		r.Get("/versions", s.handleVersions)

		// Часть роутов спека держит под /v1 (hierarchy, relations, threads):
		// клиенты бьют именно туда, на /v3 они получали 404.
		r.Route("/v1", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(s.requireAuth)
				r.Get("/rooms/{roomId}/hierarchy", s.handleHierarchy)
				r.Get("/rooms/{roomId}/relations/{eventId}", s.handleRelations)
				r.Get("/rooms/{roomId}/relations/{eventId}/{relType}", s.handleRelations)
				r.Get("/rooms/{roomId}/relations/{eventId}/{relType}/{eventType}", s.handleRelations)
				r.Get("/rooms/{roomId}/threads", s.handleThreads)
				r.Get("/rooms/{roomId}/threads/{threadId}", s.handleThread)
			})
		})

		// v3 и устаревший r0 - один набор маршрутов: Synapse отвечает на r0 так же,
		// как на v3 (Complement TestRoomState и старые SDK).
		clientV3 := func(r chi.Router) {
			r.With(s.requireAuth).Get("/capabilities", s.handleCapabilities)
			r.With(s.requireAuth).Post("/user/{userId}/openid/request_token", s.handleOpenIDRequestToken)
			r.Get("/publicRooms", s.handlePublicRooms)
			r.Post("/publicRooms", s.handlePublicRooms)
			// pushrules и turnServer требуют сессию: в спеке это пользовательские
			// данные, а вне requireAuth они всегда отдавали дефолт и пустые
			// TURN-креды.
			r.Get("/thirdparty/protocols", s.handleThirdPartyProtocols)
			s.registerPushRulesRoutes(r)
			r.Group(func(r chi.Router) {
				r.Use(s.requireAuth)
				r.Get("/voip/turnServer", s.handleTurnServer)
			})
			s.registerAuthRoutes(r)
			s.registerRoomRoutes(r)
			s.registerToDeviceRoutes(r)
			s.registerKeysRoutes(r)
			s.registerProfileRoutes(r)
			s.registerPusherRoutes(r)
			s.registerAccountDataRoutes(r)
			s.registerSearchRoutes(r)
			r.With(s.requireAuth).Post("/user_directory/search", s.handleUserDirectorySearch)
			s.registerNotificationsRoutes(r)
			s.registerEphemeralRoutes(r)

			r.Group(func(r chi.Router) {
				r.Use(s.requireAuth)
				// Лимит по устройству; long-poll выдерживается, а не отклоняется.
				r.With(s.syncRateLimit).Get("/sync", s.handleSync)
				// 3PID (почта, телефон) не поддерживаются: список пуст. Element запрашивает
				// его в настройках, 404 давал ошибку в консоли.
				r.Get("/account/3pid", func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, http.StatusOK, map[string]any{"threepids": []any{}})
				})
				r.Get("/room_keys/version", s.handleGetRoomKeysVersion)
				r.Post("/room_keys/version", s.handleCreateRoomKeysVersion)
				r.Get("/room_keys/version/{version}", s.handleGetRoomKeysVersion)
				r.Put("/room_keys/version/{version}", s.handleUpdateRoomKeysVersion)
				r.Delete("/room_keys/version/{version}", s.handleDeleteRoomKeysVersion)

				for _, path := range []string{"/room_keys/keys", "/room_keys/keys/{roomId}", "/room_keys/keys/{roomId}/{sessionId}"} {
					r.Get(path, s.handleGetRoomKeys)
					r.Put(path, s.handlePutRoomKeys)
					r.Delete(path, s.handleDeleteRoomKeys)
				}
				r.Post("/user/{userId}/filter", s.handleCreateFilter)
				r.Get("/user/{userId}/filter/{filterId}", s.handleGetFilter)
			})
		}

		r.Route("/v3", clientV3)
		r.Route("/r0", clientV3)

		// Simplified sliding sync (MSC4186): единственный способ синхронизации
		// Element X; поддержку клиент узнает по unstable_features в /versions.
		// Лимит - как у /sync, long-poll выдерживается.
		r.Route("/unstable/org.matrix.simplified_msc3575", func(r chi.Router) {
			r.Use(s.requireAuth)
			r.With(s.syncRateLimit).Post("/sync", s.handleSlidingSync)
		})

		// MSC3391 (удаление account data) клиенты зовут по unstable-префиксу: те же
		// обработчики, что и DELETE в v3 (Complement msc3391).
		r.Route("/unstable/org.matrix.msc3391", func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Delete("/user/{userId}/account_data/{eventType}", s.handleDeleteAccountData)
			r.Delete("/user/{userId}/rooms/{roomId}/account_data/{eventType}", s.handleDeleteRoomAccountData)
		})
	})

	s.router = r
	s.opsRouter = s.newOpsRouter()

	return s
}

// newPushActions собирает сервис хранимых действий push-правил и счетчиков
// непрочитанного поверх БД; логика - в core.PushActionsService.
// Вызывается из New; интеграционные тесты пересоздают сервис,
// имитируя новый процесс.
func (s *Server) newPushActions(db *store.DB) *core.PushActionsService {
	return core.NewPushActionsService(db, core.PushActionsOptions{
		Members:   store.NewSyncStore(db),
		Ephemeral: s.ephemeral,
		Lock:      s.pushUserLocks.Lock,
		SyncWait:  s.pushSyncWait,
		Warn: func(ctx context.Context, msg string, err error, args ...any) {
			if err != nil {
				args = append(args, "error", err)
			}

			s.log.WarnContext(ctx, "api: "+msg, args...)
		},
	})
}

// newOpsRouter - служебный листенер (server.ops_addr): метрики,
// подробные пробы, pprof по server.pprof.
func (s *Server) newOpsRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(s.recoverer)
	r.Get("/health", s.handleOpsHealth)
	r.Get("/health/ready", s.handleOpsReady)
	r.Get("/metrics", s.handleMetrics)

	if s.cfg.Server.Pprof {
		r.HandleFunc("/debug/pprof/", pprof.Index)
		r.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		r.HandleFunc("/debug/pprof/profile", pprof.Profile)
		r.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		r.HandleFunc("/debug/pprof/trace", pprof.Trace)
		r.Handle("/debug/pprof/{name}", http.HandlerFunc(pprof.Index))
	}

	return r
}

// OpsHandler - обработчик служебного листенера (server.ops_addr).
func (s *Server) OpsHandler() http.Handler {
	return s.opsRouter
}

// Handler возвращает готовый http.Handler (для http.Server и тестов).
func (s *Server) Handler() http.Handler {
	return s.router
}

// handleNotFound отдаёт JSON-ошибку в формате Matrix для /_matrix/* и обычный
// 404 для остальных путей.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if isMatrixPath(r.URL.Path) {
		writeMatrixError(w, http.StatusNotFound, ErrCodeUnrecognized, "Unrecognized request")

		return
	}

	http.Error(w, "404 page not found", http.StatusNotFound)
}

func (s *Server) handleMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	if isMatrixPath(r.URL.Path) {
		writeMatrixError(w, http.StatusMethodNotAllowed, ErrCodeUnrecognized, "Unrecognized request")

		return
	}

	http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
}
