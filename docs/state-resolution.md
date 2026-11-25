# State resolution v2 и v2.1

Порт Matrix State Resolution v2 (комнаты v10/v11) и v2.1 (MSC4297, комнаты v12 и
`org.matrix.hydra.11`) в `internal/state`. Эталон поведения - `synapse/state/v2.py` (pin
40f63e0); спецификация - Matrix room version 10/12.

## Алгоритм (порт synapse/state/v2.py)

Вход - **наборы состояний** (`[]StateMap`: состояние после каждого forward extremity или
каждого prev_event) и события этих наборов вместе с их **полными auth chains**. Плоский
список «текущее состояние + новые события» (прежняя реализация) не различал предка и потомка
одного ключа и не видел auth difference.

1. **Разбиение** по ключу `(type, state_key)`: ключ бесконфликтный, только если он есть во
   **всех** наборах с одним и тем же событием; ключ, которого нет хотя бы в одном наборе, -
   конфликтный (`_seperate`).
2. **Auth difference** - события, которые входят в полные auth chains одних наборов, но не
   всех (объединение минус пересечение; цепочка набора включает его события).
   `full_conflicted_set` = конфликтующие события + auth difference; для v2.1 еще
   **conflicted subgraph** - события auth DAG между конфликтующими событиями (предки одного
   конфликтующего события, у которых в auth chain есть другое).
3. **Reverse topological power sort** power-событий (`create`, `power_levels`, `join_rules`,
   kick/ban - `member` с leave/ban чужого пользователя; собственный leave и knock - не power)
   вместе с их auth-предками из `full_conflicted_set`: алгоритм Кана по auth-графу, готовые
   узлы - в куче с предвычисленным ключом `(-power отправителя, origin_server_ts, event_id)`.
   Power отправителя - по `auth_events` самого события: создатели v12 - бесконечность, без
   power_levels создатель - 100, остальные - 0.
4. **Первый проход iterative auth checks** по этому порядку поверх базового состояния: v2 -
   бесконфликтное, v2.1 - пустое. Auth-события - `auth_events` события (кроме отклоненных),
   перекрытые разрешенным состоянием по `auth_types_for_event` (join_rules - для
   join/invite/knock; create в v12 входит неявно).
5. **Mainline** от разрешенного `m.room.power_levels` по цепочке power_levels в
   `auth_events`; остальные события сортируются по `(глубина ближайшего power_levels в
   mainline, origin_server_ts, event_id)`.
6. **Второй проход iterative auth checks**, поверх результата кладется бесконфликтное
   состояние.

`state.Authorized` - проверка события по состоянию до него (state before), `state.StateAfter`
- состояние после события.

## Differential-эталоны

`internal/state/synapse_diff_test.go` - все 7 кейсов `StateTestCase` из
`tests/state/test_v2.py` без изменений: `ban_vs_pl`, `join_rule_evasion`, `offtopic_pl`,
`topic_basic`, `topic_reset`, `topic`, `mainline_sort`. Граф событий строится как
`build_event_graph` (состояние перед событием с несколькими prev - разрешение их наборов),
проверка - как `do_check`: состояние в END, отфильтрованное до ключей, изменившихся с START
или названных в expected, **равно** expected (лишний ключ - ошибка). В прежней версии тест
сравнивал только ключи из expected, кейс `topic_reset` на деле был `test_topic_basic`, и
`join_rule_evasion`, `offtopic_pl`, настоящий `topic_reset` не проходили незаметно.

`internal/state/synapse_units_test.go` - `LexicographicalTestCase`,
`SimpleParamStateTestCase`, `AuthChainDifferenceTestCase` (3 кейса),
`test_get_power_level_for_sender`, conflicted subgraph и базовое состояние v2.1.
`internal/state/state_test.go` - сценарии аудита кода (бан против join, self-leave
против бана, invite от не-участника, kick админа модератором, ключ только в одном наборе) и
2000 конфликтующих ключей меньше чем за 2 с (было около 105 с).

## Состояние на событие и forward extremities (миграция 0044)

- `state_groups` / `state_group_entries` - состояние как state groups Synapse: дельта к
  `prev_group` (цепочка не длиннее 50, не больше 100 ключей) или полный снимок;
  `event_state_groups` - состояние **после** события (для отклоненного и не-state - до
  него) и признак `rejected`; `room_forward_extremities` - события без потомков;
  `room_state_groups` - группа, совпадающая с `room_state`.
- Локальные события (`RoomStore.AppendEvent`, `CreateRoom`) пишут дельту к группе комнаты,
  сдвигают extremities (prev_events события перестают быть extremities) под
  advisory-блокировкой комнаты. Любая запись `room_state` снимает `room_state_groups`
  триггером: пути, которые знают группу, ставят ее заново, остальные (импорт, пересчет,
  прямые upsert) - нет, и следующее локальное событие пишет снимок `room_state`.
- Комната без extremities, но с `room_state` (записана до 0044 или импортом), получает
  extremity на последнем событии со снимком `room_state`.

## Интеграция (`core.ApplyRoomEvents`)

Для каждого входящего события комнаты (по depth): состояние до него - разрешение наборов
состояний после его prev_events (один prev - его состояние, prev без записанного
состояния - текущее `room_state`); событие проверяется auth rules по этому состоянию
(`state.Authorized`) и при отказе помечается `rejected`, в состояние и extremities не
попадает. Затем extremities сдвигаются, текущее состояние - разрешение наборов после всех
extremities с полными auth chains (рекурсивно по `auth_events`), `room_state` и
`room_memberships` пишутся по итогу одной транзакцией под блокировкой комнаты.

- `/send` (`internal/api/federation_send.go`): все PDU транзакции, включая не-state, по
  комнатам; догнанные `get_missing_events` предки - до исходных PDU
  (`federation_gap.go`).
- `send_join` / `send_leave` (входящие): событие после записи.
- Исходящий join (`ApplyRemoteJoin`, `internal/store/remote_join_state.go`): присланный
  state - состояние до join (разрешается с локальным состоянием на prev join или
  `room_state`, если есть), join кладется поверх без повторной проверки.
- Тесты: `internal/core/stateres_test.go` (потомок с отстающими часами, ban с отстающими
  часами, join из ветки до бана, отклонение по состоянию до события, слияние веток),
  интеграционные `TestFederationStateSetsBanVsFork` и `TestLocalEventsStateGroups`
  (`internal/api/state_sets_integration_test.go`).
- Метрики (`internal/state/metrics.go`, `GET /metrics`): время разрешения
  (`syngo_state_resolution_duration_seconds_total`), размер conflicted set
  (`syngo_state_resolution_conflicts_total`), изменённые ключи
  (`syngo_state_changed_total`), число разрешений; конфликты по комнатам -
  `Metrics.ConflictsByRoom` и лог `federation: state resolution applied`.

## Границы

- Отклоненные по состоянию события остаются в `events` (видны в timeline); хранение
  отклоненных отдельно, soft-fail по текущему состоянию и обработка по PDU.
- prev без записанного состояния (outlier, события до 0044) - опора на текущее
  `room_state`, а не `/state_ids` у пира.
- prev_events локальных событий - по-прежнему последние события по stream_ordering, а не
  forward extremities.
- Auth difference считается по полным auth chains в памяти (без chain cover index
  Synapse): разрешение загружает цепочки всех событий наборов.
- Сверка с живым Synapse на стенде не проводилась: эталоны - тесты Synapse.
