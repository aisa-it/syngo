package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/store"
)

// Лимиты транзакции /send (спецификация S2S: не больше 50 PDU и 100 EDU; PDU -
// не больше 65536 байт; Synapse: не больше 20 prev_events и 10 auth_events).
const (
	maxPDUsPerTxn    = 50
	maxPDUSize       = 65536
	maxPDUPrevEvents = 20
	maxPDUAuthEvents = 10
)

// federationSendRequest — тело PUT /_matrix/federation/v1/send/{txnId}.
type federationSendRequest struct {
	Origin string            `json:"origin"`
	PDUs   []json.RawMessage `json:"pdus"`
	EDUs   []json.RawMessage `json:"edus"`
}

// pduResult - результат обработки PDU в ответе /send: пусто - принят (в том
// числе отклонен auth rules или soft-failed, как в Synapse), error - не принят.
type pduResult struct {
	Error string `json:"error,omitempty"`
}

// handleFederationSend — PUT /_matrix/federation/v1/send/{txnId}: прием транзакции.
//
// Каждый PDU обрабатывается отдельно (Synapse _handle_pdus_in_txn): плохой PDU
// получает error в pdus.<event_id>, остальные принимаются, ответ - 200. Раньше
// один плохой PDU давал 400/403/500 на всю транзакцию, и отправитель повторял ее
// бесконечно. Транзакции одного origin обрабатываются по очереди; повтор
// (origin, txn_id) - 200 без повторного применения.
func (s *Server) handleFederationSend(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	auth, err := signing.ParseMatrixAuth(r.Header.Get("Authorization"))
	if err != nil {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Unauthorized federation request")

		return
	}

	txnID, ok := pathParam(r, "txnId")
	if !ok || txnID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid transaction id")

		return
	}

	var req federationSendRequest
	if !decodeFederationTxn(w, r, &req) {
		return
	}

	origin := req.Origin
	if origin == "" {
		origin = auth.Origin
	}

	if origin != auth.Origin {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "origin does not match signature")

		return
	}

	// Synapse: больше 50 PDU или 100 EDU - 400 на всю транзакцию.
	if len(req.PDUs) > maxPDUsPerTxn || len(req.EDUs) > maxEDUsPerTxn {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Transaction PDU or EDU count too large")

		return
	}

	// PDU - объект: без него нет ни event_id для ответа, ни комнаты (структурно
	// битая транзакция).
	for _, raw := range req.PDUs {
		if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Malformed pdu")

			return
		}
	}

	ctx := r.Context()

	unlock := s.federationTxnLocks.Lock(origin)
	defer unlock()

	seen, err := s.db.FederationTxnSeen(ctx, origin, txnID)
	if err != nil {
		s.log.ErrorContext(ctx, "api: federation inbox check failed", "origin", origin, "txn_id", txnID, "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to apply transaction")

		return
	}

	if seen {
		s.log.InfoContext(ctx, "api: duplicate federation transaction", "origin", origin, "txn_id", txnID)
		writeJSON(w, http.StatusOK, map[string]any{"pdus": map[string]pduResult{}})

		return
	}

	results, err := s.processTxnPDUs(ctx, origin, req.PDUs)
	if err != nil {
		// Сбой хранилища: транзакция не отмечена, отправитель повторит; уже
		// принятые события повтор пропустит.
		s.log.ErrorContext(ctx, "api: apply federation transaction failed", "origin", origin, "txn_id", txnID, "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to apply transaction")

		return
	}

	s.applyFederationEDUs(ctx, origin, req.EDUs)

	if _, err := s.db.SeenInbox(ctx, origin, txnID); err != nil {
		s.log.ErrorContext(ctx, "api: federation inbox record failed", "origin", origin, "txn_id", txnID, "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"pdus": results})
}

// decodeFederationTxn читает тело транзакции (до maxFederationBodySize: раньше
// тело резалось на 256 КиБ при заявленном 1 МиБ).
func decodeFederationTxn(w http.ResponseWriter, r *http.Request, target *federationSendRequest) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFederationBodySize)).Decode(target); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not parse request body")

		return false
	}

	return true
}

// txnPDU - PDU транзакции после разбора.
type txnPDU struct {
	pdu store.FedPDU
	err error
}

// processTxnPDUs обрабатывает PDU транзакции по комнатам (в комнате - по порядку
// DAG) и возвращает результат по каждому. Ошибка - только сбой хранилища.
func (s *Server) processTxnPDUs(ctx context.Context, origin string, raws []json.RawMessage) (map[string]pduResult, error) {
	results := make(map[string]pduResult, len(raws))

	var rooms []string

	byRoom := map[string][]store.FedPDU{}

	for _, raw := range raws {
		parsed, ok := s.parseTxnPDU(ctx, raw)
		if !ok {
			continue
		}

		if parsed.err != nil {
			s.log.WarnContext(ctx, "api: pdu rejected", "origin", origin, "event_id", parsed.pdu.EventID, "error", parsed.err)
			results[parsed.pdu.EventID] = pduResult{Error: parsed.err.Error()}

			continue
		}

		// Событие чужого сервера принимается только как join или invite (их
		// пересылает резидентный сервер), остальное - отбрасывается (Synapse
		// _handle_received_pdu, «Discarding PDU from invalid origin»).
		if domainOf(parsed.pdu.Sender) != origin && !relayedMembership(parsed.pdu) {
			s.log.InfoContext(ctx, "api: pdu from invalid origin discarded", "origin", origin, "event_id", parsed.pdu.EventID)
			results[parsed.pdu.EventID] = pduResult{}

			continue
		}

		roomID := parsed.pdu.RoomID
		if _, ok := byRoom[roomID]; !ok {
			rooms = append(rooms, roomID)
		}

		byRoom[roomID] = append(byRoom[roomID], parsed.pdu)
	}

	fetcher := s.newGapFetcher(origin)
	accepted := false

	for _, roomID := range rooms {
		pdus := byRoom[roomID]

		roomErr, err := s.roomAcceptsPDUs(ctx, roomID, origin)
		if err != nil {
			return results, err
		}

		if roomErr != nil {
			s.log.WarnContext(ctx, "api: room pdus dropped", "origin", origin, "room_id", roomID, "error", roomErr)

			for i := range pdus {
				results[pdus[i].EventID] = pduResult{Error: roomErr.Error()}
			}

			continue
		}

		ordered := sortByDAG(pdus)

		for i := range ordered {
			pdu := &ordered[i]
			err := fetcher.receiveEvent(ctx, *pdu, false)

			var permanent pduError
			if errors.As(err, &permanent) {
				s.log.WarnContext(ctx, "api: pdu not accepted", "origin", origin, "event_id", pdu.EventID, "error", err)
				results[pdu.EventID] = pduResult{Error: permanent.Error()}

				continue
			}

			if err != nil {
				return results, err
			}

			results[pdu.EventID] = pduResult{}
			accepted = true
		}
	}

	// Федеративные события тоже будят long-poll.
	if accepted {
		s.eventsWritten(ctx)
	}

	return results, nil
}

// roomAcceptsPDUs - PDU комнаты принимаются: m.room.server_acl не запрещает origin
// (Synapse check_server_matches_acl, иначе ошибка по каждому PDU комнаты), наш
// сервер в комнате (иначе Synapse игнорирует PDU). Ошибка-результат - roomErr.
func (s *Server) roomAcceptsPDUs(ctx context.Context, roomID, origin string) (roomErr, err error) {
	allowed, err := s.serverACLAllowed(ctx, roomID, origin)
	if err != nil {
		return nil, err
	}

	if !allowed {
		return pduError{"Server is banned from room"}, nil
	}

	in, err := s.db.HostInRoom(ctx, roomID, s.cfg.Matrix.ServerName)
	if err != nil {
		return nil, err
	}

	if !in {
		return pduError{"Server is not in room"}, nil
	}

	return nil, nil
}

// relayedMembership - join или invite: их присылает не сервер отправителя.
func relayedMembership(pdu store.FedPDU) bool {
	if pdu.Type != "m.room.member" {
		return false
	}

	membership := membershipOf(pdu.Content)

	return membership == "join" || membership == "invite"
}

// pduError - PDU не принят (ошибка в ответе /send, транзакция - 200).
type pduError struct{ msg string }

func (e pduError) Error() string { return e.msg }

// parseTxnPDU разбирает и проверяет PDU транзакции. ok=false - PDU пропущен без
// записи в ответ (нет room_id или комната нам неизвестна, как в Synapse).
func (s *Server) parseTxnPDU(ctx context.Context, raw json.RawMessage) (txnPDU, bool) {
	object, err := decodePDUObject(raw)
	if err != nil {
		return txnPDU{}, false
	}

	roomID, _ := object["room_id"].(string)
	if roomID == "" {
		return txnPDU{}, false
	}

	version := s.roomVersionOf(ctx, roomID)
	if version == 0 {
		s.log.InfoContext(ctx, "api: pdu for unknown room ignored", "room_id", roomID)

		return txnPDU{}, false
	}

	pdu, err := s.prepareRemotePDU(ctx, raw, object, version)

	return txnPDU{pdu: pdu, err: err}, pdu.EventID != ""
}

// decodePDUObject - PDU как объект JSON (числа - json.Number, как пришли).
func decodePDUObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}

	if object == nil {
		return nil, errInvalidPDU
	}

	return object, nil
}

// prepareRemotePDU проверяет PDU другого сервера и возвращает событие с
// вычисленным event_id (Synapse event_from_pdu_json + _check_sigs_and_hash):
// размер, event_id по содержимому (присланный обязан совпадать), число prev и
// auth, подписи серверов отправителя, content hash - при несовпадении сохраняется
// редактированная копия. Ошибка с непустым EventID - PDU не принят.
func (s *Server) prepareRemotePDU(ctx context.Context, raw []byte, object map[string]any, version int) (store.FedPDU, error) {
	wire := make(map[string]any, len(object))

	for key, value := range object {
		if key != "event_id" {
			wire[key] = value
		}
	}

	eventID, err := core.EventID(wire, version)
	if err != nil {
		return store.FedPDU{}, err
	}

	fail := func(msg string) (store.FedPDU, error) {
		return store.FedPDU{EventID: eventID}, pduError{msg}
	}

	compact := compactPDU(raw)
	if len(compact) > maxPDUSize {
		return fail("PDU too large")
	}

	// event_id в v3+ считает получатель: присланный и не совпавший - подмена
	// (раньше id брался из PDU как есть).
	if claimed, present := object["event_id"]; present && claimed != eventID {
		return fail("event_id does not match event content")
	}

	eventType, _ := wire["type"].(string)
	sender, _ := wire["sender"].(string)

	if eventType == "" || domainOf(sender) == "" {
		return fail("PDU without type or sender")
	}

	if len(stringSlice(wire["prev_events"])) > maxPDUPrevEvents {
		return fail("Too many prev_events")
	}

	// Без auth_events - только m.room.create.
	if auth := stringSlice(wire["auth_events"]); (len(auth) == 0 && eventType != "m.room.create") || len(auth) > maxPDUAuthEvents {
		return fail("Invalid auth_events")
	}

	for _, server := range pduSigners(wire) {
		if err := eventjson.VerifyVersioned(wire, server, version, s.federationKeyLookup(ctx)); err != nil {
			return fail("Invalid signature of " + server)
		}
	}

	hashes, _ := wire["hashes"].(map[string]any)
	expected, _ := hashes["sha256"].(string)

	if expected == "" {
		return fail("PDU without content hash")
	}

	actual, err := signing.ContentHash(wire)
	if err != nil {
		return fail("Cannot compute content hash")
	}

	stored := []byte(compact)

	// Подпись верна, а content hash нет: content изменен по дороге - сохраняется
	// редактированная копия (как Synapse); event_id от этого не меняется.
	if actual != expected {
		s.log.WarnContext(ctx, "api: pdu content hash mismatch, redacting", "event_id", eventID)

		stored, err = core.MarshalPDU(redactedPDU(wire, version))
		if err != nil {
			return fail("Cannot redact PDU")
		}
	}

	pdu, err := parseFederationPDU(stored, version)
	if err != nil {
		return fail("Malformed PDU")
	}

	if pdu.EventID != eventID {
		return fail("event_id does not match event content")
	}

	return pdu, nil
}

// redactedPDU - редактированная копия PDU по правилам версии комнаты: поля,
// которые сохраняет redaction (в v1-v10 - и origin, membership, prev_state), и
// content, урезанный по типу события. event_id копии тот же.
func redactedPDU(wire map[string]any, version int) map[string]any {
	redacted := eventjson.RedactVersioned(wire, version)

	if version <= 10 {
		for _, key := range []string{"origin", "membership", "prev_state"} {
			if value, ok := wire[key]; ok {
				redacted[key] = value
			}
		}
	}

	return redacted
}

// pduSigners - серверы, чья подпись PDU обязательна: сервер отправителя, а у join
// через restricted-комнату - и сервер, авторизовавший вход (спецификация, v8+).
func pduSigners(object map[string]any) []string {
	sender, _ := object["sender"].(string)
	signers := []string{domainOf(sender)}

	if eventType, _ := object["type"].(string); eventType != "m.room.member" {
		return signers
	}

	content, _ := object["content"].(map[string]any)

	if membership, _ := content["membership"].(string); membership == "join" {
		if via, _ := content["join_authorised_via_users_server"].(string); via != "" {
			if server := domainOf(via); server != "" && server != signers[0] {
				signers = append(signers, server)
			}
		}
	}

	return signers
}

// sortByDAG упорядочивает события комнаты так, чтобы prev и auth события из той
// же транзакции шли раньше ссылающихся на них (при равенстве - по depth и порядку
// в транзакции): событие, авторизованное соседом по транзакции, иначе
// проверялось бы до записи своего auth-события.
func sortByDAG(pdus []store.FedPDU) []store.FedPDU {
	index := make(map[string]int, len(pdus))
	for i := range pdus {
		index[pdus[i].EventID] = i
	}

	order := make([]int, len(pdus))
	for i := range order {
		order[i] = i
	}

	sort.SliceStable(order, func(a, b int) bool { return pdus[order[a]].Depth < pdus[order[b]].Depth })

	out := make([]store.FedPDU, 0, len(pdus))
	state := make([]uint8, len(pdus)) // 0 - не посещено, 1 - в обходе, 2 - готово

	var visit func(i int)

	visit = func(i int) {
		if state[i] != 0 {
			return
		}

		state[i] = 1

		for _, ref := range append(append([]string(nil), pdus[i].Auth...), pdus[i].Prev...) {
			if j, ok := index[ref]; ok && j != i {
				visit(j)
			}
		}

		state[i] = 2
		out = append(out, pdus[i])
	}

	for _, i := range order {
		visit(i)
	}

	return out
}

// parseFederationPDU разбирает входящее событие (без записи в БД) и вычисляет его
// event_id: в комнатах v3+ id считает получатель (reference hash по redacted-копии
// с правилами версии); присланное поле event_id, не совпавшее с
// вычисленным, - ошибка.
func parseFederationPDU(raw json.RawMessage, version int) (store.FedPDU, error) {
	var pdu struct {
		EventID  string          `json:"event_id"`
		RoomID   string          `json:"room_id"`
		Type     string          `json:"type"`
		Sender   string          `json:"sender"`
		StateKey *string         `json:"state_key"`
		Content  json.RawMessage `json:"content"`
		Prev     []string        `json:"prev_events"`
		Auth     []string        `json:"auth_events"`
		Depth    int64           `json:"depth"`
		OriginTS int64           `json:"origin_server_ts"`
		Redacts  string          `json:"redacts"`
		// Подписи и hashes - в колонки events (аудит, старый путь отдачи);
		// в федерацию уходит исходный PDU целиком.
		Signatures json.RawMessage `json:"signatures"`
		Hashes     json.RawMessage `json:"hashes"`
	}

	if err := json.Unmarshal(raw, &pdu); err != nil {
		return store.FedPDU{}, err
	}

	object, err := decodePDUObject(raw)
	if err != nil {
		return store.FedPDU{}, errInvalidPDU
	}

	delete(object, "event_id")

	if version <= 0 {
		version = core.DefaultRoomVersion
	}

	id, err := eventjson.EventID(object, version)
	if err != nil {
		return store.FedPDU{}, errInvalidPDU
	}

	if pdu.EventID != "" && pdu.EventID != id {
		return store.FedPDU{}, fmt.Errorf("%w: event_id %s, computed %s", errInvalidPDU, pdu.EventID, id)
	}

	// v12: room_id create выводится из его event_id, create не в auth_events.
	if roomID, err := v12RoomIDOf(raw, id, version); err != nil {
		return store.FedPDU{}, errInvalidPDU
	} else if roomID != "" {
		pdu.RoomID = roomID
	}

	if pdu.RoomID == "" {
		return store.FedPDU{}, errInvalidPDU
	}

	// Исходный PDU - как пришел (без незначащих пробелов): раньше событие из /send
	// хранилось без signatures, hashes, origin и unsigned, и в send_join/state оно
	// уходило без подписи.
	return store.FedPDU{
		EventID: id, RoomID: pdu.RoomID, Type: pdu.Type, Sender: pdu.Sender, Redacts: pdu.Redacts,
		StateKey: pdu.StateKey, Content: []byte(pdu.Content), Prev: pdu.Prev, Auth: pdu.Auth,
		Depth: pdu.Depth, OriginTS: pdu.OriginTS,
		Signatures: nullRaw(pdu.Signatures), Hashes: nullRaw(pdu.Hashes), PDU: compactPDU(raw),
	}, nil
}

// nullRaw - отсутствующее или null-поле JSON как пустое значение.
func nullRaw(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	return raw
}

// registerFederationSendRoutes подключает входящие федеративные send-маршруты.
func (s *Server) registerFederationSendRoutes(r chi.Router) {
	r.Put("/_matrix/federation/v1/send/{txnId}", s.handleFederationSend)
}

// errInvalidPDU — событие без room_id или с чужим event_id.
var errInvalidPDU = errors.New("api: invalid pdu")
