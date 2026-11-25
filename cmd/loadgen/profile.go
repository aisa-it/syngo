package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Duration - time.Duration в JSON строкой ("30s", "5m").
type Duration time.Duration

// MarshalJSON пишет длительность строкой.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON принимает строку ("90s") или число секунд.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("длительность %q: %w", s, err)
		}

		*d = Duration(v)

		return nil
	}

	var secs float64
	if err := json.Unmarshal(data, &secs); err != nil {
		return fmt.Errorf("длительность: ожидали строку вида \"30s\" или число секунд, получили %s", data)
	}

	*d = Duration(time.Duration(secs * float64(time.Second)))

	return nil
}

// D - значение как time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Режимы задания нагрузки.
const (
	// modeUsers - закрытая модель: N активных пользователей, между действиями
	// пауза (think time, экспоненциальное распределение со средним think_time).
	modeUsers = "users"
	// modeRPS - открытая модель: действия с целевой частотой target_rps (в сумме по
	// всем пользователям) независимо от того, успевает ли сервер.
	modeRPS = "rps"
)

// Действия пользователя (ключи weights).
const (
	actSend          = "send"
	actTyping        = "typing"
	actReceipt       = "receipt"
	actMessages      = "messages"
	actKeysQuery     = "keys_query"
	actKeysUpload    = "keys_upload"
	actMediaUpload   = "media_upload"
	actMediaDownload = "media_download"
	actCreateRoom    = "create_room"
	actJoinLeave     = "join_leave"
	actInvite        = "invite"
)

// knownActions - все действия; порядок - для стабильного вывода.
var knownActions = []string{
	actSend, actTyping, actReceipt, actMessages, actKeysQuery, actKeysUpload,
	actMediaUpload, actMediaDownload, actCreateRoom, actJoinLeave, actInvite,
}

// Profile - параметры нагрузки. Встроенные S/M/L - builtinProfiles; свой - JSON
// (-profile-file) поверх встроенного base.
type Profile struct {
	Name string `json:"name"`
	// Base - встроенный профиль, поверх которого применяется JSON (по умолчанию S).
	Base string `json:"base,omitempty"`

	// Users - размер пула пользователей (у каждого одно устройство и свой /sync).
	Users int `json:"users"`
	// Rooms - комнаты, создаваемые при подготовке (дальше растут create_room).
	Rooms int `json:"rooms"`
	// RoomsPerUser - в скольких комнатах профиля состоит пользователь.
	RoomsPerUser int `json:"rooms_per_user"`
	// RoomSkew - показатель Ципфа популярности комнат: 0 - равномерно, 1 - несколько
	// больших комнат и длинный хвост маленьких (как в живом развертывании).
	RoomSkew float64 `json:"room_skew"`
	// EncryptedRoomsPercent - доля комнат с m.room.encryption: сообщения в них -
	// m.room.encrypted с фиктивным шифротекстом (сервер его не расшифровывает).
	EncryptedRoomsPercent int `json:"encrypted_rooms_percent"`

	Mode string `json:"mode"`
	// ThinkTime - средняя пауза между действиями пользователя (mode users).
	ThinkTime Duration `json:"think_time"`
	// TargetRPS - действий в секунду суммарно (mode rps).
	TargetRPS float64 `json:"target_rps"`
	// MaxInflight - предел одновременных действий в mode rps: сверх него действие
	// не выполняется и считается dropped (генератор не копит очередь).
	MaxInflight int `json:"max_inflight"`

	// Ramp - плавный рост: пользователи подключаются (initial sync) равномерно, а
	// частота действий растет линейно от 0 до цели.
	Ramp Duration `json:"ramp"`
	// Warmup - от старта нагрузки до начала замера (включает ramp).
	Warmup Duration `json:"warmup"`
	// Duration - длительность замера.
	Duration Duration `json:"duration"`
	// Drain - после замера действия останавливаются, /sync еще ждет доставки.
	Drain Duration `json:"drain"`

	// SyncTimeout - timeout long-poll /sync (Element - 30 с).
	SyncTimeout Duration `json:"sync_timeout"`
	// TimelineLimit - timeline.limit фильтра /sync.
	TimelineLimit int `json:"timeline_limit"`
	// LazyLoadMembers - lazy_load_members в фильтре, как у Element.
	LazyLoadMembers bool `json:"lazy_load_members"`

	// MessageBytes - размер тела текстового сообщения.
	MessageBytes int `json:"message_bytes"`
	// MediaKB - средний размер загружаемого файла (±50%).
	MediaKB int `json:"media_kb"`

	// Weights - доли действий (сумма любая, нормируется).
	Weights map[string]float64 `json:"weights"`

	// SetupConcurrency - параллельность подготовки (регистрация, комнаты, входы).
	SetupConcurrency int `json:"setup_concurrency"`
	// Seed - зерно случайности: одинаковое зерно - одинаковые членства и порядок.
	Seed int64 `json:"seed"`
}

// defaultWeights - смесь действий клиента вроде Element: чаще всего пишут и
// читают, реже - медиа, ключи и комнаты.
func defaultWeights() map[string]float64 {
	return map[string]float64{
		actSend:          40,
		actTyping:        15,
		actReceipt:       15,
		actMessages:      6,
		actKeysQuery:     6,
		actKeysUpload:    2,
		actMediaUpload:   3,
		actMediaDownload: 6,
		actCreateRoom:    1,
		actJoinLeave:     3,
		actInvite:        3,
	}
}

// builtinProfiles - встроенные профили. Средняя частота действий в mode users -
// users / think_time: S ~10/с, M ~100/с, L ~500/с; send - 40% из них. При
// think_time 5 с отправка у пользователя ~0,08/с - ниже прод-лимита rc_message
// (0,2/с, burst 10), поэтому 429 на send - признак проблемы, а не профиля.
func builtinProfiles() map[string]Profile {
	base := Profile{
		RoomSkew:              1.0,
		EncryptedRoomsPercent: 30,
		Mode:                  modeUsers,
		MaxInflight:           256,
		SyncTimeout:           Duration(30 * time.Second),
		TimelineLimit:         20,
		LazyLoadMembers:       true,
		MessageBytes:          120,
		MediaKB:               64,
		SetupConcurrency:      8,
		Seed:                  166,
	}

	s := base
	s.Name, s.Users, s.Rooms, s.RoomsPerUser = "S", 50, 20, 5
	s.ThinkTime = Duration(5 * time.Second)
	s.Ramp, s.Warmup = Duration(15*time.Second), Duration(20*time.Second)
	s.Duration, s.Drain = Duration(60*time.Second), Duration(10*time.Second)
	s.TargetRPS = 10

	m := base
	m.Name, m.Users, m.Rooms, m.RoomsPerUser = "M", 500, 200, 10
	m.ThinkTime = Duration(5 * time.Second)
	m.Ramp, m.Warmup = Duration(60*time.Second), Duration(90*time.Second)
	m.Duration, m.Drain = Duration(10*time.Minute), Duration(15*time.Second)
	m.TargetRPS, m.MaxInflight, m.SetupConcurrency = 100, 512, 16

	l := base
	l.Name, l.Users, l.Rooms, l.RoomsPerUser = "L", 5000, 1000, 15
	l.ThinkTime = Duration(10 * time.Second)
	l.Ramp, l.Warmup = Duration(5*time.Minute), Duration(6*time.Minute)
	l.Duration, l.Drain = Duration(30*time.Minute), Duration(30*time.Second)
	l.TargetRPS, l.MaxInflight, l.SetupConcurrency = 500, 2048, 32

	s.Weights, m.Weights, l.Weights = defaultWeights(), defaultWeights(), defaultWeights()

	return map[string]Profile{"S": s, "M": m, "L": l}
}

// builtinProfile возвращает встроенный профиль по имени (без учета регистра).
func builtinProfile(name string) (Profile, error) {
	p, ok := builtinProfiles()[strings.ToUpper(name)]
	if !ok {
		return Profile{}, fmt.Errorf("неизвестный профиль %q (встроенные: S, M, L; свой - -profile-file)", name)
	}

	return p, nil
}

// parseProfile разбирает JSON-профиль поверх встроенного base (по умолчанию S).
// Неизвестные поля - ошибка: опечатка в имени поля иначе молча дала бы дефолт.
func parseProfile(data []byte) (Profile, error) {
	var head struct {
		Base string `json:"base"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Profile{}, fmt.Errorf("профиль: %w", err)
	}

	if head.Base == "" {
		head.Base = "S"
	}

	p, err := builtinProfile(head.Base)
	if err != nil {
		return Profile{}, err
	}

	// Веса в JSON заменяют встроенные целиком: иначе убрать действие нельзя.
	var weights struct {
		Weights map[string]float64 `json:"weights"`
	}
	if err := json.Unmarshal(data, &weights); err != nil {
		return Profile{}, fmt.Errorf("профиль: %w", err)
	}

	if weights.Weights != nil {
		p.Weights = nil
	}

	p.Name = "custom"

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&p); err != nil {
		return Profile{}, fmt.Errorf("профиль: %w", err)
	}

	p.Base = strings.ToUpper(head.Base)
	err = p.Validate()

	return p, err
}

// loadProfile - встроенный профиль name или JSON из path (path важнее).
func loadProfile(name, path string) (Profile, error) {
	if path == "" {
		p, err := builtinProfile(name)
		if err != nil {
			return Profile{}, err
		}

		err = p.Validate()

		return p, err
	}

	data, err := os.ReadFile(path) //nolint:gosec // путь к профилю задает оператор
	if err != nil {
		return Profile{}, fmt.Errorf("профиль: %w", err)
	}

	return parseProfile(data)
}

// Validate проверяет согласованность профиля.
func (p *Profile) Validate() error {
	var errs []error

	if p.Users < 2 {
		errs = append(errs, errors.New("users >= 2: доставку меряем между пользователями"))
	}

	if p.Rooms < 1 {
		errs = append(errs, errors.New("rooms >= 1"))
	}

	if p.RoomsPerUser < 1 || p.RoomsPerUser > p.Rooms {
		errs = append(errs, fmt.Errorf("rooms_per_user в [1, rooms=%d], получили %d", p.Rooms, p.RoomsPerUser))
	}

	if p.RoomSkew < 0 || p.RoomSkew > 3 {
		errs = append(errs, errors.New("room_skew в [0, 3]"))
	}

	if p.EncryptedRoomsPercent < 0 || p.EncryptedRoomsPercent > 100 {
		errs = append(errs, errors.New("encrypted_rooms_percent в [0, 100]"))
	}

	switch p.Mode {
	case modeUsers:
		if p.ThinkTime <= 0 {
			errs = append(errs, errors.New("think_time > 0 в mode users"))
		}
	case modeRPS:
		if p.TargetRPS <= 0 || p.MaxInflight < 1 {
			errs = append(errs, errors.New("target_rps > 0 и max_inflight >= 1 в mode rps"))
		}
	default:
		errs = append(errs, fmt.Errorf("mode %q: users или rps", p.Mode))
	}

	if p.Duration <= 0 {
		errs = append(errs, errors.New("duration > 0"))
	}

	if p.Ramp < 0 || p.Warmup < 0 || p.Drain < 0 {
		errs = append(errs, errors.New("ramp, warmup, drain >= 0"))
	}

	if p.Ramp > p.Warmup {
		errs = append(errs, fmt.Errorf("ramp (%s) не длиннее warmup (%s): замер на неполной нагрузке",
			p.Ramp.D(), p.Warmup.D()))
	}

	if p.SyncTimeout < 0 || p.SyncTimeout.D() > 5*time.Minute {
		errs = append(errs, errors.New("sync_timeout в [0, 5m]"))
	}

	if p.TimelineLimit < 1 || p.MessageBytes < 1 || p.MediaKB < 1 || p.SetupConcurrency < 1 {
		errs = append(errs, errors.New("timeline_limit, message_bytes, media_kb, setup_concurrency >= 1"))
	}

	total := 0.0

	for name, w := range p.Weights {
		if !isKnownAction(name) {
			errs = append(errs, fmt.Errorf("weights: неизвестное действие %q (есть: %s)", name, strings.Join(knownActions, ", ")))
		}

		if w < 0 {
			errs = append(errs, fmt.Errorf("weights.%s < 0", name))
		}

		total += w
	}

	if total <= 0 {
		errs = append(errs, errors.New("weights: сумма весов > 0"))
	}

	return errors.Join(errs...)
}

func isKnownAction(name string) bool {
	for _, a := range knownActions {
		if a == name {
			return true
		}
	}

	return false
}

// sortedWeights - веса в порядке knownActions (для вывода и выбора).
func (p *Profile) sortedWeights() []weighted {
	out := make([]weighted, 0, len(p.Weights))

	for _, name := range knownActions {
		if w := p.Weights[name]; w > 0 {
			out = append(out, weighted{name: name, weight: w})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].weight > out[j].weight })

	return out
}

// expectedActionRate - ожидаемая частота действий на полной нагрузке, в секунду.
func (p *Profile) expectedActionRate() float64 {
	if p.Mode == modeRPS {
		return p.TargetRPS
	}

	return float64(p.Users) / p.ThinkTime.D().Seconds()
}
