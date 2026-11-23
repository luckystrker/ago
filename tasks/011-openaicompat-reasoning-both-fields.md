---
id: 011
title: OpenAI-совместимые провайдеры — reasoning из reasoning_content и reasoning в стриме и не-стриме, один ModelReasoning
status: done
priority: 2
mode: parallel
depends_on: []
wave: G
human: none
upstream: 7a65080d
decisions: none
executor_model: opencode-go/deepseek-v4.1-flash
verifier_model: claude-sonnet-5-5
owner: exec-deepseek011
---
## Что сделать
Upstream `7a65080d` (`git -C <ag2> show 7a65080d -- ag2/config/openai/`): серверы расходятся в имени поля —
DeepSeek, SGLang, xAI, DashScope шлют `reasoning_content`; vLLM, OpenRouter, Ollama — `reasoning`.
У нас в общем движке `config/openaicompat.go` та же асимметрия:
- не-стрим читает `wireChoice.Message.Reasoning` (`json:"reasoning"`, ~стр. 889) и не смотрит `reasoning_content`;
- стрим читает `Delta.ReasoningContent` (`json:"reasoning_content"`, ~стр. 1069/1126) и не смотрит `reasoning`.

Шаги (red-first):
1. Табличный тест на 4 клетки «диалект × режим» через `httptest`-фейк: не-стрим с `reasoning_content` и стрим с
   `reasoning` сейчас теряют reasoning — красные (стрим-тест на `reasoning_content` уже есть, не дублировать).
2. Один помощник `extractReasoning` для message и delta: сначала `reasoning_content`, потом `reasoning`.
3. Сервер заполнил **оба** поля → один `events.ModelReasoning` на ответ (в стриме — без удвоения дельт). Тест на это.
4. Учти, что Mistral-ветка (`splitWireContent`, ~стр. 901) тоже даёт reasoning из content-чанков: не удвоить и не сломать
   её тесты (`config/mistral`).

## Финишная черта тикета
- [ ] Вывод табличного теста до (красные клетки) и после (4/4 + «оба поля → один ModelReasoning»). — артефакт: оба вывода.
- [ ] **Р:** программа в `$BB_THREAD_STORAGE`: агент через публичный OpenAI-совместимый конфиг против `httptest`-сервера,
  отвечающего по очереди в 4 вариантах, печатает число и текст событий `ModelReasoning` в истории. До: 0 в двух
  вариантах; после: ровно 1 в каждом. — артефакт: оба вывода.
- [ ] Мутация «приоритет `reasoning` над `reasoning_content`» или «оба поля → два события» — тест краснеет. — артефакт: вывод.

## Поведенческое изменение / совместимость
- Пользователи vLLM/OpenRouter/Ollama в стриме и DeepSeek/xAI в не-стриме начнут получать `ModelReasoning`, которого
  раньше не было (больше событий в истории, reasoning может уходить обратно в провайдер по существующим правилам).
  Публичный API не меняется. Строка в MIGRATION «AG2 v1.3.4 sync» (заведи раздел, если его ещё нет); запись в DECISIONS не нужна — это паритет.

## Швы для тестов
- `httptest`-сервер в формате chat-completions (SSE для стрима) + публичный конфиг провайдера.

## Границы / не трогать
- Только `config/openaicompat.go` и его тесты; другие провайдеры (`config/anthropic`, `config/gemini`, …), `acp/`, `toolpacks/`, `a2ui/`, `knowledge/`, `network/`.
- Публичный API не ломать; stdlib only; `config` не импортирует `agent`.

## Правила AGo (обязательны, нарушение = возврат)
- Прочитай `AGENTS.md`, `docs/ARCHITECTURE.md` (граф зависимостей пакетов) и раздел
  «Синк v1.3.4 (2026-11-23)» в `UPSTREAM_CHANGES.md` (только читать).
- **База:** `main` (последняя запись решений — D-131; D-129/D-130 закреплены за тикетом 004). Worktree исполнителя
  форкается от свежего `main` — ничего подтягивать не нужно. Записи DECISIONS дописывай в конец файла.
- **Не нагружай машину** (просьба владельца, VPS общий): Go-команды — `GOTOOLCHAIN=go1.24.13 GOMAXPROCS=2 nice -n 19 go ...`,
  тесты `-p 1`; сначала только затронутые пакеты, полный `go test ./...` — один раз перед review. TLC вручную не запускать.
  Известные флейки под нагрузкой — в `docs/known-issues.md`; упал — повтори только его, приложи оба вывода.
- **Python-эталон:** `/home/piagent/.bb-machines/luckystriker.getbb.app/checkouts/ago2/ag2`
  (общий клон, рабочее дерево на старом коммите — **не делай checkout**). Читай так:
  `git -C <ag2> show <sha>` (дифф коммита) и `git -C <ag2> show v1.3.4:<путь>` (файл на теге).
- Только stdlib, **никаких новых внешних зависимостей** (нужна — стоп, сообщи тимлиду).
- Без импорт-циклов (`agent` не импортирует `network`, `config` не импортирует `agent`).
- `context.Context` — первый параметр отменяемых операций; ошибки — пакетные сентинелы
  (`var ErrX = errors.New(...)`), оборачивание через `%w`, проверка `errors.Is/As`; без паник.
- Экспортируемые типы с изменяемым состоянием — безопасны для конкурентного использования (и это задокументировано).
- Публичный API не ломать без записи в `docs/DECISIONS.md` и строки в `docs/MIGRATION.md` (раздел «AG2 v1.3.4 sync» —
  заводит первый тикет, которому он нужен; остальные дописывают в него, второй такой раздел не заводить).
- Любое отступление от Python — комментарий `// AGO-DEVIATION: <причина>` в коде + запись в
  `docs/DECISIONS.md` (номер D закреплён за тикетом в его тексте; перед записью `grep -n '^## D-<N>' docs/DECISIONS.md` — пусто).
- Комментарии объясняют ограничения, не историю («ported from…» — нельзя).
- gofmt проверяй **по содержимому из git**, не по файлу на диске (CRLF-чек-аут слеп):
  `git diff --name-only <база> -- '*.go' | xargs -I{} sh -c 'git show :{} | gofmt -l' ` после `git add`,
  или `gofmt -l` на файлах с LF.
- Гейты — под тулчейном CI: `GOTOOLCHAIN=go1.24.13` (на VPS 1.27, `-race` недоступен).
- **Red-first:** на каждый пункт сначала тест, который падает на текущем коде (вывод падения — в отчёт), потом правка, тот же тест зелёный.
- Тесты: stdlib `testing`, без сети (fake-клиенты, `httptest`), без `t.Skip`.
- **Не коммитить.** Коммит делает тимлид после вердикта проверяющего. Если просят
  подготовить список файлов — только явные пути (никогда `git add -A` / `git add .`).
- Протокол доски: `B="node ~/.agent-toolkit/bin/board.mjs"`; `$B claim <id> --as <имя>` →
  `$B log "..." --as <имя> --ticket <id>` → `$B status <id> review --as <имя> --note "артефакты: ..."`.
  В `done` переводит только проверяющий (другая модель); при `human: required` — только владелец.
- Отчёт приёмки: `docs/acceptance/v1.3.4-<id>.md` — что сделано, команды гейтов с выводом, артефакт «Р».

## Готовность
- **Г:** `go build ./...`, `go vet ./...`, `go test -count=1 ./...` из корня (минимум — затронутые
  пакеты, но перед review — полный прогон), `go build ./examples/...`, `go vet ./examples/...`, `gofmt` чист.
- **Р:** живой прогон из «Финишной черты» с артефактом в отчёте приёмки. Без «Р» тикет не закрывается.
