---
id: 003
title: DecisionSet — несколько вопросов одним запросом (ядро response + OpenAI Decisions)
status: done
priority: 2
mode: parallel
depends_on: []
wave: B
human: none
upstream: 1c3e0c52f8
decisions: D-128
executor_model: claude-sonnet-5-5
verifier_model: opencode-go/deepseek-v4.1-flash
owner: exec-sonnet-003
---
## Что сделать
Upstream `1c3e0c52f8` (ADR 0038, дополняет 0018/0023): плоский класс в `response_schema` = «по вопросу на поле»,
уходит **одним** запросом (общий вход оплачивается один раз). `git -C <ag2> show 1c3e0c52f8 -- ag2/response/ ag2/config/`.

**1. Ядро `response/`.** Сейчас `response/decision.go` знает один вопрос (`QuestionSchema`, `FromResponse` с конвертом
`{"data": ...}`); `response/schema.go` уже строит JSON Schema из struct.
- Скаляр (bool/enum/int-шкала) — как раньше, один вопрос с ключом `"answer"`; существующие вызовы `FromResponse` не ломаются.
- Плоский struct → набор `(ключ, DecisionSpec)` в порядке полей; ключ — имя из тега `json`; текст вопроса — из тега
  (предложи `question:"..."`, запасной — `description`, как читает `schema.go`).
- **Отказ до запроса** (сентинел, например `ErrInvalidDecisionSet`, через `%w`, с именем поля): вложенный struct, указатель
  (аналог `Optional`), срез/массив/map, пустой struct, коллизия ключей (одинаковый `json`, встраивание) и всё, что отклоняют
  Python-тесты коммита. Python-отказы без Go-аналога (значения по умолчанию, вопрос на самом классе) — перечислить в D-128 с решением.
- `Render`/`metadata` = `{поле: ответ}` в порядке полей. Имена идиоматичные (`NewDecisionSet`), набор неизменяем и безопасен для конкурентного использования.

**2. `config/openai/decisions.go`.** struct в `ResponseSchema` → все вопросы одним HTTP-запросом; ответ → `metadata`;
отказ **любого** поля → `*DecisionRefusedError{Name: <поле>}` (тип есть); больше 400 полей — ошибка до запроса;
скаляр — запрос байт-в-байт как раньше. `config/typesafe` — заглушка (`ErrNotImplemented`), не трогать.

**3. Документы.** `docs/DECISIONS.md` **D-128** (аналог ADR 0038: правило, источник текста вопроса, отказы, отступления,
лимит 400, typesafe — при порте клиента); `docs/PORT_STATUS.md` — строки `response/` и openai decisions; `docs/MIGRATION.md` §11 —
строка «новое, не ломает»; пример в `docs/GUIDE.md`, если там есть раздел про decisions.

## Финишная черта тикета
- [ ] Табличный тест на **каждый** отказ из Python-тестов + положительные (1 поле, N полей, enum, шкала, порядок) — артефакт: вывод `go test -run DecisionSet -v ./response/` и таблица «Python-тест → Go-тест».
- [ ] `httptest`: 3 поля → ровно 1 запрос с 3 вопросами; отказ 2-го поля → `errors.As` даёт его ключ; 401 поле → ошибка, 0 запросов; скаляр — тело запроса как до правки. — артефакт: вывод `go test -run Decision -v ./config/openai/`.
- [ ] **Р:** программа в `$BB_THREAD_STORAGE` против `httptest`-фейка Decisions API: struct из 3 полей (bool, enum, шкала) — печать захваченного тела запроса, числа запросов (1) и `metadata`; struct с вложенным полем — печать ошибки с именем поля. Пример из D-128/GUIDE запускается тем же способом. — артефакт: вывод `go run`.

## Швы для тестов
- Публичный конструктор набора + `Render`; провайдер — через `httptest` и публичный клиент.

## Не трогать
- Пакеты тикета 001 (`config/anthropic`, `config/gemini`, `agui`, `acp`, `observers`, `usage`, `eval`, `middleware`, `contrib/otelx`), `agent/` (002), `config/typesafe/`.

## Правила AGo (обязательны, нарушение = возврат)
- Прочитай `AGENTS.md`, `docs/ARCHITECTURE.md` (граф зависимостей пакетов) и раздел
  «Синк v1.3.2 (2026-11-16)» в `UPSTREAM_CHANGES.md` (только читать).
- **База:** ветка `bb/ag2-v1-3-2-sync-thr_umfdz4azq5`. Worktree исполнителя форкается от
  `main` — первым шагом `git merge bb/ag2-v1-3-2-sync-thr_umfdz4azq5`.
- **Python-эталон:** `/home/piagent/.bb-machines/luckystriker.getbb.app/checkouts/ago2/ag2`
  (общий клон, рабочее дерево на старом коммите — **не делай checkout**). Читай так:
  `git -C <ag2> show <sha>` (дифф коммита) и `git -C <ag2> show v1.3.2:<путь>` (файл на теге).
- Только stdlib, **никаких новых внешних зависимостей** (нужна — стоп, сообщи тимлиду).
- Без импорт-циклов (`agent` не импортирует `network`, `config` не импортирует `agent`).
- `context.Context` — первый параметр отменяемых операций; ошибки — пакетные сентинелы
  (`var ErrX = errors.New(...)`), оборачивание через `%w`, проверка `errors.Is/As`; без паник.
- Экспортируемые типы с изменяемым состоянием — безопасны для конкурентного использования (и это задокументировано).
- Любое отступление от Python — комментарий `// AGO-DEVIATION: <причина>` в коде + запись в
  `docs/DECISIONS.md` (номер D закреплён за тикетом в его тексте; запись дописывается в конец файла).
- Комментарии объясняют ограничения, не историю («ported from…» — нельзя).
- gofmt проверяй **по содержимому из git**, не по файлу на диске (CRLF-чек-аут слеп):
  `git diff --name-only <база> -- '*.go' | xargs -I{} sh -c 'git show :{} | gofmt -l' ` после `git add`,
  или `gofmt -l` на файлах с LF.
- Гейты — под тулчейном CI: `GOTOOLCHAIN=go1.24.13` (на VPS 1.27, `-race` недоступен).
- Тесты: stdlib `testing`, без сети (fake-клиенты, `httptest`), без `t.Skip`.
- **Не коммитить.** Коммит делает тимлид после вердикта проверяющего. Если просят
  подготовить список файлов — только явные пути (никогда `git add -A` / `git add .`).
- Протокол доски: `B="node ~/.agent-toolkit/bin/board.mjs"`; `$B claim <id> --as <имя>` →
  `$B log "..." --as <имя> --ticket <id>` → `$B status <id> review --as <имя> --note "артефакты: ..."`.
  В `done` переводит только проверяющий (другая модель).
- Отчёт приёмки: `docs/acceptance/v1.3.2-<id>.md` — что сделано, команды гейтов с выводом, артефакт «Р».

## Готовность
- **Г:** `go build ./...`, `go vet ./...`, `go test -count=1 ./...` из корня (минимум — затронутые
  пакеты, но перед review — полный прогон), `gofmt` чист; для contrib — те же три команды внутри модуля.
- **Р:** живой прогон из «Финишной черты» с артефактом в отчёте приёмки. Без «Р» тикет не закрывается.
