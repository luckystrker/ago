---
id: 001
title: Единая семантика usage — prompt_tokens = весь вход (кэш), thinking в completion, без двойного счёта у потребителей
status: open
priority: 1
mode: parallel
depends_on: []
wave: A
human: none
upstream: 8464369e40
decisions: D-126
executor_model: opencode-go/deepseek-v4.1-flash
verifier_model: claude-sonnet-5-5
---
## Что сделать
Upstream `8464369e40` (ADR 0036, **breaking**): `Usage.prompt_tokens` = весь вход на любом провайдере,
включая чтения и записи prompt-cache; `total` не меньше «вход + выход». Python поправил мапперы, снял
пер-провайдерную поправку AG-UI и перестал исключать кэш в TokenMonitor/eval/метриках/телеметрии
(`git -C <ag2> show 8464369e40 --stat`, затем по файлам). Делать в таком порядке — каждый шаг отдельным тестом:

**1. Anthropic.** `config/anthropic/mappers.go` → `NormalizeUsage` (~стр. 1122): сейчас `PromptTokens = InputTokens`
без кэша. Нужно `PromptTokens = input + cache_read + cache_creation` (поля кэша остаются разбивкой), `TotalTokens`
— от полного входа. Кэш различается **по присутствию, а не по истинности**: присланный `0` = `0`, отсутствующее = `nil`.
Слияние `message_start`+`message_delta` в стриме (`config/anthropic/client.go` ~765–790) должно дать то же, что батч.
Bedrock — заглушка (`Create()` → `ErrNotImplemented`), его половину не делаем.

**2. Gemini (батч).** `config/gemini/mappers.go` → `normalizeUsage` (~стр. 638): `CompletionTokens = candidates + thoughts`
(thinking остаётся в `ThinkingTokens`), `TotalTokens` меньше «вход + выход» не принимается (точное правило —
`git -C <ag2> show v1.3.2:ag2/config/gemini/mappers.py`; Live-вариант того же — `367119e83a`). Старое отступление D-022
в коде снять. Проверить, что стрим Gemini идёт через ту же функцию.

**3. Потребители — убрать досчёт кэша поверх prompt** (после шага 1 он станет двойным):
`agui/stream.go` ~500–518 (удалить `cacheOutsidePrompt` и досложение в `inputTotal`), `acp/provider_client.go` ~166
и `acp/serve.go` ~818 (сверить с Python `acp/mappers.py`), `observers/token_monitor.go` ~200, `usage/usage.go` ~96,
`eval/scorers.go` ~58, `eval/trace.go` ~249, `middleware/metrics.go` ~472, `contrib/otelx/otelx.go` ~138
(`gen_ai.usage.input_tokens`). Плюс `grep -rn "CacheReadInputTokens\|CacheCreationInputTokens"` по всему дереву с
`examples/`. OpenAI/openaicompat/Ollama/Responses: подтвердить, что уже отдают «весь вход» (у OpenAI `prompt_tokens` включает cached).

**4. Документы.** `docs/DECISIONS.md` **D-126** (аналог ADR 0036: правило, провайдеры, снятая поправка AG-UI, Bedrock —
при порте; к D-022 дописать «superseded by D-126», саму не переписывать). `docs/MIGRATION.md` — новый раздел
**«11. AG2 v1.3.2 sync»**, подпункт «Breaking: prompt_tokens включает кэш»: что вырастет (`TokenMonitor` пороги, `UsageReport`,
`otelx`, метрики, Gemini completion), числа из «Р», рецепт старого числа (`prompt - cache_read - cache_creation`),
«следующий релиз — минорный (0.11.0)». `docs/PORT_STATUS.md` — убрать «anthropic/bedrock cache correction».

## Финишная черта тикета
- [ ] Таблицы тестов: Anthropic (нет кэша / read / creation / оба / присланные нули / стрим start+delta), Gemini (без thinking / с thinking / `toolUsePromptTokenCount` / total от API меньше суммы / nil остаётся nil). — артефакт: вывод `go test -run Usage -v ./config/anthropic/ ./config/gemini/`.
- [ ] Таблица всех потребителей «изменён / уже верно — почему». — артефакт: в отчёте приёмки.
- [ ] **Р — сквозной прогон до/после.** Программа в `$BB_THREAD_STORAGE` (не в репозитории): `httptest`-фейк Anthropic отвечает
  `{input_tokens:100, cache_read_input_tokens:900, cache_creation_input_tokens:50, output_tokens:20}`; агент с `TokenMonitor`, отчётом `usage`,
  AG-UI потоком и `otelx` (in-memory экспортёр). Печатает: `UsageEvent` prompt/total (батч и стрим), TokenMonitor, UsageReport, AG-UI `input`,
  атрибут спана. На базовом коммите и после: **после — prompt 1050 / total 1070 везде, нигде 1950**. Второй прогон — Gemini-фейк
  `prompt 100, candidates 20, thoughts 30, total 150`: до `completion=20`, после `completion=50 thinking=30 total=150`. — артефакт: обе таблицы до/после.
- [ ] **Р — документы:** каждое число и имя функции в D-126/MIGRATION сверено с кодом (таблица «утверждение → файл:строка»); сниппет миграции запускается (`go run`, вывод в отчёте).

## Швы для тестов
- `NormalizeUsage` / `normalizeUsage` — чистые функции; стрим — `httptest` SSE-фейк через публичный клиент.
- Потребители — через их публичные выходы (`ObserverAlert`, `UsageReport`, `RUN_FINISHED`, атрибуты спана), не через приватные помощники.

## Не трогать
- `agent/` (тикет 002), `response/`, `config/openai/decisions.go` (003), `toolpacks/`, новые `contrib/sandbox*` (004).
- Если тикет не влезает в одну сессию — сдай шаги 1–2 на review и скажи тимлиду; 3–4 тем же тредом следующим заходом.

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
