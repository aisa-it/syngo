# Complement: baseline и ratchet

Здесь лежит версионируемый baseline официального
тест-сьюта [Complement](https://github.com/matrix-org/complement) и allowlist исключений.

- `results.jsonl` — нормализованные результаты последнего принятого прогона: по строке
  `{"Action":"pass|fail|skip","Test":"..."}`, отсортировано по имени теста; первая строка -
  `# measured: <дата> pass N/M` (если baseline записан ratchet-ом). Файл обновляется только
  через проверку (`scripts/complement.sh --update-baseline` или `--adopt-results`): при
  регрессии, меньшем числе pass или сокращении набора тестов baseline не меняется.
- `allowlist.txt` — исключения из ratchet: `<тест> | <причина> | <дата пересмотра>`.
  Исключение без причины или с просроченной датой валит проверку.

Прогон 25 ноября 2026 года повторил результаты 21 ноября. Из 553 зарегистрированных
проверок прошли 340, завершились ошибкой 206, пропущены 7. На верхнем уровне прошли
88 из 142. Ещё 77 сценариев федерации остались без результата после остановки тестового
клиента. HTTPS на порту 8448 в тестовом образе недоступен.

Для прогона задан `COMPLEMENT_ENABLE_DIRTY_RUNS=1` (один homeserver на пакет).
В списке исключений остаются `TestFilter` и `TestContent`. Пустой baseline,
пустой прогон или отсутствие ранее зарегистрированного теста завершают проверку с ошибкой.

Обновление пина Complement (тегов у проекта нет, пин — коммит): взять SHA из
`https://api.github.com/repos/matrix-org/complement/commits?per_page=1`, поменять
`COMPLEMENT_REF` в `scripts/complement.sh`, прогнать и осознанно обновить baseline.

## Взять baseline из артефакта CI (без Docker)

Workflow Complement запускается вручную и сохраняет результаты в файле
`complement-results.jsonl`. Чтобы принять его как baseline, выполните команду

```bash
scripts/complement.sh --adopt-results=/путь/к/complement-results.jsonl
```

Файл проходит ту же проверку ratchet, что и прогон: с регрессиями, меньшим числом pass
или сокращенным набором тестов baseline не обновляется. Раньше файл копировался поверх
baseline при одном `pass` (исправлено).

Проверка ratchet без Docker:

```bash
python3 -m unittest discover -s tools/complement -p 'test_*.py'
```

## Troubleshooting прогона

- **Процесс завершается сразу после загрузки модулей, без тестов.** Чаще всего это OOM на
  компиляции Complement (6 GiB / 4 vCPU) или ошибка сборки тестового пакета. Скрипт теперь:
  ограничивает параллелизм компиляции (`COMPLEMENT_GOFLAGS=-p=2` по умолчанию), делает
  `go vet` по тестовым пакетам до запуска (`COMPLEMENT_PREBUILD=1`), пишет полный лог в
  `/tmp/complement-go-test.log` и печатает его хвост при ненулевом коде. Если в JSON-выводе
  нет ни одного `pass/fail/skip`, скрипт завершается с кодом `go test` и явным сообщением,
  а не «молча зелёным».
- **`set -euo pipefail` маскировал вывод.** Запуск `go test` обёрнут в `set +e` с сохранением
  кода, поэтому причина видна в логе; при необходимости поднимите лимиты:
  `COMPLEMENT_GOFLAGS=-p=1`, больше RAM/своп, или соберите тесты заранее.
- **Baseline.** `test/complement/results.jsonl` наполняется первым успешным прогоном:
  `scripts/complement.sh --update-baseline`. Проверка ratchet:
  `python3 tools/complement/complement_ratchet.py check --input <log.jsonl>`.
