# Чейнджлог оригинального AG2 (upstream)

> Закоммиченный файл слежения за upstream AG2 — обновляется по пайплайну ниже
> при каждой проверке (в т.ч. когда новых версий нет).
> Пайплайн: (1) проверить последний релиз — `gh release list --repo ag2ai/ag2 --limit 5`;
> (2) если релиз новее шапки — обновить шапку, сравнить теги
> `gh api repos/ag2ai/ag2/compare/<старый-тег>...<новый-тег>` и по каждому коммиту
> дать отчёт: id / автор / вид правки / описание / нужно ли утягивать
> (багфиксы — сверить поведение с нашей версией и взять при расхождении;
> фичи — по умолчанию берём; правки логики — только если соответствует идиомам Go);
> (3) если нового релиза нет — записать «новой версии нет» и обновить
> приложение с висящими коммитами main;
> (4) при волновой разработке (формат синка v0.8.0) — два жёстких правила из
> постмортема релиза: (a) gofmt-гейт гонять по содержимому из git
> (`git show HEAD:<файл> | gofmt -l /dev/stdin` либо LF-копии изменённых
> файлов) — локальный `gofmt -l` на CRLF-чек-ауте слеп к реальным отклонениям
> и молча пропустит то, что уронит CI; (b) `git add -A` в коммитах волн
> запрещён — коммитить только явный список файлов от разработчика, иначе
> рабочий мусор агентов (как `.mutlogs/` в v0.8.0) попадает в релиз.
>
> `gh` в окружении нет: релизы и коммиты читаются через `curl` + GitHub REST API
> (`/releases/latest`, `/releases/tags/<tag>`, `/compare/<base>...<head>`), при
> заданном `GITHUB_TOKEN`/`GH_TOKEN` — с `Authorization: Bearer`. Без токена
> лимит 60 запросов/час, поэтому патчи берутся из ответа `compare`.

## Актуальная версия (бейслайн)

**v1.3.0** — <https://github.com/ag2ai/ag2/releases/tag/v1.3.0> (релиз 2026-11-04,
тег = `1229064a23a1e39a4c3c089bb4643a637117c8a9`).
Последняя проверка: **2026-11-09**.
Последний просмотренный SHA main: `5125cb5dfcc99aa3c31ca248cb734c63628a99dd`
(12 коммитов после v1.3.0, разобраны ниже — без нового релиза).

## Новые версии

**v1.3.0** (2026-11-04) — новый релиз, первый после бейслайна v1.2.2. Крупный:
наблюдение за деревом делегирования (`ChildEvent`, `origin`), явные области
видимости переменных (`ScopedValue`: COPIED/LOCAL/SHARED), единый жизненный цикл
задач для запусков агента и делегирований, миграция knowledge на плагины,
`cache_creation_extended_input_tokens`, realtime-TTS Mistral, deprecations к 1.4.0
(`TaskConfig`/`Agent(tasks=)`, `Agent(assembly=)`, `Agent(knowledge=)`, `Agent.task()`)
и требование Python 3.11+.

Затем main ушёл на 12 коммитов вперёд (до `5125cb5dfc`, 2026-11-09) — они разобраны
в разделе «main после v1.3.0».

## Синк v1.3.0 (2026-11-09)

**Дата проверки:** 2026-11-09. **Диапазон:** `v1.2.2...v1.3.0` (41 коммит) плюс
main после релиза до `5125cb5dfc` (12 коммитов). Из 41 коммита релиза 24 уже были
разобраны в синке v0.8.0 (ниже по файлу) — их правки вошли в v1.3.0 без изменений
и повторно не разбираются; новых для разбора — 17.

Изменения сгруппированы по темам. Для каждого коммита: id / автор / вид правки /
что и зачем / вердикт «утягивать».

### Ранее разобранные коммиты, вошедшие в v1.3.0

`82bdfb480`, `4d5773e7b`, `17dea422f`+`9031481ec`, `153ef4cdd`, `32931b6ef`,
`be11666a5`, `15c2a9411`, `d2274ff2d`, `6a818894b`, `07144b1a8`+`82b15120c`,
`bd25ea97f` (skip), `78a06eddb` (skip), а также `59bba8fdd`, `8fc2c4aee`,
`d367c8206`, `c0c7acdc1` (отложено), `a21cc7db1` (отложено), `36aa5fde7`,
`14c9715ae`, `7430f438b`, `b121a308f` (служебные) — подробный разбор и статусы
см. в разделе «Синк v0.8.0». Новых действий по ним нет; два отложенных пункта
(`c0c7acdc1` — Windows-Python в skills, `a21cc7db1` — symlink-безопасность
filesystem) по-прежнему ждут портирования соответствующих зон.

### Наблюдаемость дерева делегирования

**`663ddd1574`** — [`feat(events): add observation primitives and source_event properties`](https://github.com/ag2ai/ag2/commit/663ddd1574)
- **Автор:** Pastukhov Nikita (@Lancetnik).
- **Вид:** фича (событийная модель).
- **Что изменено:** новый модуль `ag2/events/delegation.py` с примитивами
  наблюдения за потомками — `ChildEvent` (обёртка события потомка: `inner`,
  `task_id`, `path`, `agent_name`, `origin_event_id`, `direction`), его transient-
  вариант, `DeliveryFailed` и `DownEvent`/`TransientDownEvent` (аудит вмешательства
  предка). У `BaseEvent` появилось свойство `source_event` (для обычного события —
  оно само, для наблюдения — авторское событие), а `UsageEvent` получил `event_id`
  для склейки провенанса. Затронуты `events/{__init__,base,types}`, `stream.py`.
- **Утягивать:** **ДА** — базовый слой для переноса дерева событий субагентов
  (#3526). В AGo `events.Base` нет `SourceEvent`/`ChildEvent`; добавлять вместе с
  `Delivery`-маркером (см. `a591d84f6e`) и без `__conversational__`-магии —
  в Go это флаги `Transient()`/`Conversational()` у типа события.

**`fad570ca19`** — [`feat: add scoped invocation variables`](https://github.com/ag2ai/ag2/commit/fad570ca19)
- **Автор:** Pastukhov Nikita (@Lancetnik).
- **Вид:** фича (контекст/делегирование).
- **Что изменено:** новый `ag2/scopes.py`: `Scope` (`COPIED` — глубокое копирование
  на каждую ветку, дефолт для обычных значений; `LOCAL` — не наследуется потомками;
  `SHARED` — одна привязка на всё дерево вызова) и обёртка `ScopedValue`.
  `agent.py`/`context.py` применяют объявленные области при построении дочерних
  контекстов, `run_task.py` больше не копирует переменные вручную.
- **Утягивать:** **ДА** — у нас в `agent/subtask.go` дочерние переменные копируются
  вручную и плоско (`VariablesSnapshot` → `run.cx.Variables`), без областей
  видимости и без глубокого копирования. Это ровно та поверхность, которую
  описывает фича; реализуемо на stdlib (`reflect`/явный `Clone` для значений).

**`1c27f221a5`** — [`feat: integrate subagent event trees with consumer migrations`](https://github.com/ag2ai/ag2/commit/1c27f221a5)
- **Автор:** Pastukhov Nikita (@Lancetnik).
- **Вид:** фича + правка логики (крупная).
- **Что изменено:** сквозная интеграция дерева событий: новый
  `ag2/tools/subagents/_forwarding.py` (`StreamHandle`/origin, пересылка вверх),
  `run_task.py` похудел на ~230 строк, операционные события потомков приходят
  предку как `ChildEvent` (не попадают в транскрипт и не триггерят инструменты/
  модель/HITL), а `UsageEvent`/`TaskEvent` — как есть (InertEvent) с провенансом
  (`path`, `task_id`, `agent_name`, первый хоп стампит, последующие сохраняют и
  только перепривязывают живой origin-handle). Затронуты `agent.py`, `context.py`,
  `history.py`, `usage.py`, `events/types.py`, telemetry, observers
  (`loop_detector`, `token_monitor`), eval span loader, ACP/AG-UI.
- **Утягивать:** **ДА** — но большой кусок: у нас `agent/subtask.go` не пересылает
  события ребёнка родителю вовсе (только `TaskStarted/Completed` и rollup usage),
  поэтому наблюдатель на корневом стриме не видит прогресс делегаций. Портировать
  после `663ddd1574`/`fad570ca19`, опираясь на наши `stream.Stream` +
  `observers` (механика подписки уже есть); `context.Context` вместо
  асинхронного ContextVar-стека.

**`9ac7c6a70c`** — [`docs: document subagent event routing and variable scopes`](https://github.com/ag2ai/ag2/commit/9ac7c6a70c)
- **Автор:** Pastukhov Nikita (@Lancetnik).
- **Вид:** документы.
- **Что изменено:** ADR 0030–0032 (прямая пересылка событий субагентов, объявление
  SHARED-имён на корне, учёт usage потомков по source-событиям) и
  `docs/design/subagent-eventbus-v2.md`; правки ADR 0002/0003/0004/0014.
- **Утягивать:** **НЕ ПРИМЕНИМО** — документация upstream; контракт переносим
  в `docs/specs/core.md` при порте двух предыдущих коммитов.

**`a591d84f6e`** — [`chore: prepare AG2 1.3.0 eventbus support`](https://github.com/ag2ai/ag2/commit/a591d84f6e)
- **Автор:** Pastukhov Nikita (@Lancetnik).
- **Вид:** рефакторинг + релизная подготовка.
- **Что изменено:** бамп версии до 1.3.0, `GLOSSARY.md`, а главное — ad-hoc
  `setattr`-флаги (`_downlink`, `_validate_delivery`, `_bridged`) заменены типизированным
  маркером `Delivery`/`routed()`/`delivery_of()` в `events/delegation.py`;
  `scopes.py` переиспользует `RESERVED_VARIABLE_PREFIXES` вместо повторного
  литерала `("ag:", "a2a:")`.
- **Утягивать:** **НЕ ПРИМЕНИМО** как отдельный коммит (бамп/глоссарий/рефакторинг
  ещё не портированного кода). Учесть при реализации `663ddd1574`: в Go маркер
  делаем типом-обёрткой (не скрытым полем структуры), а список зарезервированных
  префиксов выносим в одну константу `context` и переиспользуем в копировании
  переменных субагента.

### Задачи, знания, live

**`d716fcbefe`** — [`feat: unify task lifecycle for agent runs and delegations`](https://github.com/ag2ai/ag2/commit/d716fcbefe)
- **Автор:** Pastukhov Nikita (@Lancetnik).
- **Вид:** фича + багфиксы жизненного цикла.
- **Что изменено:** новый `ag2/_run_lifecycle.py`; каждый запуск агента и каждое
  делегирование открывают один и тот же `Task`, терминальное событие публикуется
  всегда (в т.ч. при ошибке чтения usage, отмене и `BaseException` из инструмента),
  тайтл задачи — имя агента, а не промпт (промпты не утекают в network-hub),
  `AgentRun.__aexit__` освобождает стрим при любом исключении. Добавлены
  `Forwarder.lifecycle()` и `PARENT_CANCELLED`, ADR 0034/0035, ~840 строк тестов.
- **Утягивать:** **ДА** — у нас `Agent.Task` создаёт задачу только по явному
  вызову, и турн агента задачу не открывает (`ag2/task.py` vs наш `agent/run.go`);
  делегации эмитят `TaskStarted/Completed` вручную в `agent/subtask.go`. Унификация
  убирает расхождение и даёт гарантию «задача всегда завершается» — это идиоматично
  для Go (defer + явные терминальные переходы, без исключений).

**`8db78dd5c3`** — [`fix(task): resolve TaskInject to the enclosing task under concurrent sibling tasks`](https://github.com/ag2ai/ag2/commit/8db78dd5c3)
- **Автор:** 陈志谦 (@simpleqt).
- **Вид:** багфикс.
- **Что изменено:** `TaskInject` читал единственный слот `ag2.task` в
  `context.dependencies`, поэтому при параллельных sibling-вызовах инструментов
  код внутри раннего блока резолвился в позднюю задачу (прогресс/чекпоинты
  приписывались чужой задаче). Теперь резолв идёт по цепочке, привязанной к
  вызывающей asyncio-задаче (`ContextVar`), с проверкой conversation/stream-id и
  учётом явно переданной `ask(dependencies={"ag2.task": task})`.
- **Утягивать:** **ПОЗЖЕ** — API `TaskInject` в AGo не портирован (в
  `task/task.go` зафиксировано: слот `task.DepKey` общий для sibling-задач).
  Тот же класс дефекта есть у сетевого инструмента `tasks` (`network/clienttools.go`),
  который читает `task.DepKey` из контекста вызова. Правильное Go-решение —
  передавать активную задачу явно через `context.Context` (значение на вызов),
  а не через общий слот; делать вместе с портом `TaskInject`, иначе чиним
  несуществующую поверхность.

**`d2f9a85dab`** — [`feat: migrate knowledge to plugins and refresh deprecated API examples`](https://github.com/ag2ai/ag2/commit/d2f9a85dab)
- **Автор:** Pastukhov Nikita (@Lancetnik).
- **Вид:** фича (архитектурная миграция) + багфиксы делегирования.
- **Что изменено:** knowledge уезжает из `Agent(knowledge=...)` в
  `plugins=[KnowledgePlugin(...)]`: новые `knowledge/plugin.py`,
  `knowledge/_maintenance.py` (scoped-обслуживание), `_legacy.py`-шим, из
  `agent.py` удалено ~430 строк. Попутно: публикация running-agent для каждого
  турна (A2A/A2UI/NLIP-обслуживаемые knowledge-агенты больше не падают),
  делегирование заполняет только отсутствующие ключи конфигурации (значения
  `ask(dependencies=...)` побеждают), второй `KnowledgePlugin` и стратегия без
  триггера отклоняются на конструкторе. ADR 0033.
- **Утягивать:** **ПОЗЖЕ** — блокер архитектурный: наш `plugin.Plugin` по D-044
  не несёт `dependencies`/`variables`/`observers`, а knowledge-конфиг живёт
  middleware-обёрткой (`agent.WithKnowledgeConfig`). Переносить после расширения
  плагинов; функциональные фиксы делегирования (не затирать store воркера,
  per-request store, отклонение второго плагина) имеют смысл и отдельно.

**`22a93d852e`** — [`feat(live): speak Mistral replies over one realtime speech connection`](https://github.com/ag2ai/ag2/commit/22a93d852e)
- **Автор:** Semen Frolov (@vvlrff).
- **Вид:** фича (live/realtime TTS).
- **Что изменено:** `MistralRealtimeTTSConfig` поверх realtime speech из
  mistralai 3.6.0 и протокол `TextStreamingTTSConfig`: весь текст ответа уходит
  одним `stream_text` по мере генерации вместо запроса на каждое предложение;
  каскад закрывает соединение при ошибке/перебивании и озвучивает ответ до
  запуска его tool-call'ов. Вторая правка: `Speech.aclose()` не ждёт медленного
  (3–10 с) закрытия handshake, иначе отменённый турн ронял следующий ввод.
- **Утягивать:** **ПОЗЖЕ** — realtime-стек (`RealtimeConfig`, barge-in,
  streaming TTS, провайдеры elevenlabs/gemini/mistral) у нас отложен целиком
  (D-057), портирован только HTTP-каскад с OpenAI-стадиями. Фича целиком живёт
  на websocket-realtime, которого в AGo нет.

### Провайдеры, usage, инструменты

**`305a50c774`** — [`fix(live): use gemini-3.8-live in Gemini live example/docs and ModelName`](https://github.com/ag2ai/ag2/commit/305a50c774)
- **Автор:** alexcastgonz (@pcbeingused333).
- **Вид:** багфикс (модели Gemini Live).
- **Что изменено:** `gemini-3.1-flash-live-preview` выключается 2026-11-17;
  `ModelName` в `ag2/live/gemini.py` переведён на `gemini-3.8-live` (+
  `gemini-3.8-live-extended-thinking`), шесть снятых/снимаемых имён убраны,
  примеры и документация обновлены.
- **Утягивать:** **НЕТ** — Gemini Live в AGo не портирован (`live/` знает только
  OpenAI-стадии, D-057), а `config/gemini` принимает произвольную строку модели
  без литерала `ModelName`. Дефекта в нашей поверхности нет; при порте Gemini Live
  взять актуальный список имён.

**`362dd7074b`** — [`chore(gemini): replace retired and retiring Gemini model names in docs and tests`](https://github.com/ag2ai/ag2/commit/362dd7074b)
- **Автор:** Semen Frolov (@vvlrff).
- **Вид:** документы/тесты (переименование моделей).
- **Что изменено:** в тестах и документации снятые/несозданные ID
  (`gemini-3-flash-preview`, `gemini-3.5-flash`, `gemini-2.0-flash`,
  `gemini-2.5-pro` и др.) заменены рекомендованными (`gemini-3.6-flash`,
  `gemini-3.5-flash-lite`, `gemini-2.5-flash-lite`). Библиотечный код не менялся.
- **Утягивать:** **НЕ ПРИМЕНИМО** — только upstream-тесты и docs.

**`d503c8ddc8`** — [`fix(tools): preserve optional Tavily search result fields`](https://github.com/ag2ai/ag2/commit/d503c8ddc8)
- **Автор:** 他们都不玩了 (@shuaiyafei).
- **Вид:** багфикс.
- **Что изменено:** `ag2/tools/search/tavily.py`: в `SearchResult` и при разборе
  ответа добавлены необязательные `raw_content` и `favicon`, которые API отдаёт,
  а модель теряла.
- **Утягивать:** **ДА** — тот же дефект есть у нас: `toolpacks.TavilySearchResult`
  (`toolpacks/search.go:298`) содержит только `title/url/content/score`, поля
  `raw_content`/`favicon` отбрасываются при декодировании. Правка на 4 строки +
  тест.

**`9cfbb69e95`** — [`docs: finish status bar use cases and standalone guide`](https://github.com/ag2ai/ag2/commit/9cfbb69e95)
- **Автор:** Pastukhov Nikita (@Lancetnik).
- **Вид:** документы + примеры.
- **Что изменено:** 11 исполняемых примеров `examples/status_middleware_usecases/`
  (лимит звонка, todo-план, восстановление после ошибки, персистентный прогресс,
  retry-снимок и т.д.) и отдельная страница `status_bars.mdx`; зафиксировано, что
  persistent-статус внутри `RetryMiddleware` не поддерживается (ADR 0028), а
  status исключается из входа суммаризации.
- **Утягивать:** **НЕ ПРИМЕНИМО** — документация/examples. Полезно как источник
  сценариев для тестов нашего `middleware/statusbar.go`, но кода не требует.

**`c761fcfe43`** — [`docs: explain scoped variables and subagent event routing`](https://github.com/ag2ai/ag2/commit/c761fcfe43)
- **Автор:** Pastukhov Nikita (@Lancetnik).
- **Вид:** документы.
- **Что изменено:** пользовательские гайды по областям видимости переменных
  (`context/variables`) и маршрутизации событий субагентов (`advanced/stream`,
  `subagents`); примеры делегирования переведены на OpenAI Responses и
  регистрацию колбэков через observer-декораторы.
- **Утягивать:** **НЕ ПРИМЕНИМО** — документация; контракты переносим в
  `docs/specs/core.md` при порте `fad570ca19`/`1c27f221a5`.

**`1229064a23`** — [`docs: fix TaskEvent filter, stale depth row and ScopeError note`](https://github.com/ag2ai/ag2/commit/1229064a23)
- **Автор:** Semen Frolov (@vvlrff).
- **Вид:** документы (правки гайдов).
- **Что изменено:** исправлен фильтр `TaskEvent` в примере, устаревшая строка про
  глубину дерева и примечание про `ScopeError`. Это последний коммит, вошедший в
  тег v1.3.0.
- **Утягивать:** **НЕ ПРИМЕНИМО** — документация.

### A2UI / MCP / файлы

**`a6cb59a575`** — [`chore(deps): bump the github-actions group with 4 updates`](https://github.com/ag2ai/ag2/commit/a6cb59a575)
- **Автор:** dependabot[bot].
- **Вид:** зависимости/CI.
- **Что изменено:** версии `setup-uv`, `setup-python`, `prek-action`,
  `codeql-action` в workflow-файлах.
- **Утягивать:** **НЕ ПРИМЕНИМО** — Go-репозиторий, GitHub Actions upstream не
  используются.

**`a474cd248f`** — [`chore: require Python 3.11+ and fix Ruff checks`](https://github.com/ag2ai/ag2/commit/a474cd248f)
- **Автор:** Pastukhov Nikita (@Lancetnik).
- **Вид:** зависимости/CI + уборка совместимости.
- **Что изменено:** `requires-python >= 3.11`, снят мёртвый код для 3.10
  (`_PY310_ENUM_DOC`), `asyncio.TimeoutError` → встроенный `TimeoutError`, правки
  Ruff по всему чекауту.
- **Утягивать:** **НЕ ПРИМЕНИМО** — требование к версии Python и его stdlib.

### main после v1.3.0 (12 коммитов, `v1.3.0...5125cb5dfc`)

**`472627dccd`** — [`docs: explain Agent Status Bar with practical examples`](https://github.com/ag2ai/ag2/commit/472627dccd)
- **Автор:** Pastukhov Nikita (@Lancetnik). **Вид:** документы (blog).
- **Что изменено:** статья-обзор Agent Status Bar с картинками, публикация в блоге.
- **Утягивать:** **НЕ ПРИМЕНИМО**.

**`a612a1a39d`** — [`feat(xai): support safety_identifier and guidelines`](https://github.com/ag2ai/ag2/commit/a612a1a39d)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** фича (xAI).
- **Что изменено:** в `XAIConfig`/`CreateOptions` добавлены `safety_identifier`
  и `guidelines` (список `Guideline` из xai_sdk), прокинуты в клиент; документация
  по model_configuration.
- **Утягивать:** **ПОЗЖЕ** — наш `config/xai` — заглушка (`Create()` возвращает
  `ErrNotImplemented`, D-008, нет gRPC-транспорта). Поля без клиента бессмысленны;
  добавить вместе с портом xAI-клиента.

**`c751dabac4`** — [`feat(usage): report extended prompt-cache writes (OpenAI 12h, Anthropic 1h)`](https://github.com/ag2ai/ag2/commit/c751dabac4)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** фича (usage).
- **Что изменено:** новое поле `Usage.cache_creation_extended_input_tokens` —
  часть `cache_creation_input_tokens`, записанная с расширенным TTL: OpenAI
  Responses берёт `input_tokens_details.cache_write_tokens_details.ttl_12h_tokens`,
  Anthropic — `cache_creation.ephemeral_1h_input_tokens`; Chat Completions
  breakdown не отдаёт и оставляет `None`. Поле участвует в `__bool__` и `__add__`;
  поднят минимум openai-extra, документация `prompt_cache_options={"ttl":"12h"}`.
- **Утягивать:** **ДА** — наш `events.Usage` (`events/model.go`) знает только
  `CacheCreationInputTokens`, а `config/anthropic` и `config/openai` уже
  нормализуют usage вручную. Добавить `*int64`-поле, `Add`/`bool`-семантику,
  маппинг в двух местах и тесты; расширенный TTL тарифицируется иначе, так что
  без поля стоимость не посчитать.

**`b4814ca6e6`** — [`fix(usage): carry extended cache writes through telemetry, traces and metrics`](https://github.com/ag2ai/ag2/commit/b4814ca6e6)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс.
- **Что изменено:** новое поле терялось всеми потребителями после вызова модели:
  `TelemetryMiddleware` теперь пишет `gen_ai.usage.cache_creation_extended_input_tokens`
  (измеренный 0 сохраняется), span-loader читает атрибут обратно в `Usage`,
  `MetricsMiddleware` добавляет `token_type="cache_creation_extended_input"`
  (нули не эмитятся, как у остальных типов). Bedrock не трогают — там нельзя
  запросить cache point.
- **Утягивать:** **ДА** — в связке с `c751dabac4`: у нас usage штампуется в спаны
  в `contrib/otelx` (`contrib/otelx/otelx.go:116-150`), поле нужно добавить туда
  же, иначе оно потеряется ровно так, как описано. Метрик-мидлвари в AGo нет —
  этот кусок неприменим.

**`3539d07500`** — [`fix(gemini): send the skip placeholder for unsigned current-turn function calls`](https://github.com/ag2ai/ag2/commit/3539d07500)
- **Автор:** Mark Sze (@marklysze). **Вид:** багфикс (Gemini).
- **Что изменено:** Gemini 3 отвечает 400, если шаг function-call в текущем турне
  без `thought_signature`. mappers подставляют документированный плейсхолдер
  Google `skip_thought_signature_validator` первому вызову каждого такого шага и
  логируют предупреждение; подписанные вызовы и прошлые турны не трогаются.
- **Утягивать:** **ДА** — наш `config/gemini/mappers.go` вообще не переносит
  `thoughtSignature`: `genPart` его не читает, а `buildContents` собирает
  `functionCall` без подписи. На Gemini 3 любой реплей function-call в текущем
  турне упадёт с 400. Минимальная правка — тот же плейсхолдер; полный паритет
  (сохранение подписи из ответа и её реплей) — отдельная задача.

**`1b0c399012`** — [`fix(a2ui): speak the stable A2UI v1.0 wire and re-vendor the spec`](https://github.com/ag2ai/ag2/commit/1b0c399012)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс/правка логики (wire-протокол).
- **Что изменено:** upstream стабилизировал A2UI v1.0; AG2 1.3.0 вёз release
  candidate. Ре-вендоринг спек v1.0 и v0.9.1 (upstream `078b3d5c`): сообщения и
  capability-объекты несут голую версию `"1.0"` (протокол по-прежнему выбирается
  как `"v1.0"`), `TextField.value` обязателен в basic-каталоге, A2UI A2A-парты
  несут MIME в `metadata["mimeType"]` в дополнение к `media_type`, кастомный
  v1.0-каталог может `$ref` только объявленное (иначе ошибка на загрузке,
  как у upstream `run_tests.py`), capability v0.9.1 читаются под ключом `"v0.9"`.
- **Утягивать:** **ДА** — наш `a2ui` использует `VersionV1 = "v1.0"` и как
  селектор протокола, и как версию в каждом сообщении (`a2ui/constants.go`,
  `Message.Version`), т.е. после стабилизации отдаёт невалидный wire. Нужно
  развести «протокол `v1.0`» и «версию сообщения `1.0`», обновить вендоренную
  спеку/каталог и добавить обязательность `TextField.value`. Это wire-breaking
  изменение — делать отдельной волной с golden-тестами (у нас они есть).

**`da5dbe1b38`** — [`feat(mcp): let browser hosts connect, explain MCP Apps misuse, and add a quick start`](https://github.com/ag2ai/ag2/commit/da5dbe1b38)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** фича (MCP serving).
- **Что изменено:** браузерные хосты могут подключаться к MCP-серверу (CORS-
  рецепты, современные routing-заголовки), в `ag2/mcp/{apps,server,tools}.py`
  расширена работа с MCP Apps, документация объясняет типичное misuse и получает
  quick start.
- **Утягивать:** **ПОЗЖЕ** — MCP Apps UI/SEP-2133 в `mcpserver` не портированы
  (PORT_STATUS: «Not ported … MCP Apps UI/SEP-2133»), CORS/браузерных хостов тоже
  нет. Брать вместе с портом Apps-слоя.

**`9e7575ae6d`** — [`fix(openai): accept streamed tool calls without an index`](https://github.com/ag2ai/ag2/commit/9e7575ae6d)
- **Автор:** alexcastgonz (@pcbeingused333). **Вид:** багфикс.
- **Что изменено:** OpenAI-совместимые серверы (например, Gemini) опускают
  `tool_call.index` в стриминговых дельтах, из-за чего накопление падало с
  `TypeError`. Теперь новый вызов начинается, когда дельта несёт `id`, иначе
  продолжается последний; добавлены тесты и на индексированный путь.
- **Утягивать:** **ДА** — у нас тот же дефект в `config/openaicompat.go:693-698`:
  при `tc.Index == nil` берётся `ix = 0`, поэтому несколько параллельных вызовов
  без `index` схлопнутся в первый аккумулятор. Нужно: `nil index` + есть `id` →
  новый аккумулятор; иначе — последний. Тест на форму чанков Gemini.

**`9885a93014`** — [`chore(deps): bump provider SDKs in uv.lock`](https://github.com/ag2ai/ag2/commit/9885a93014)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** зависимости.
- **Что изменено:** anthropic 2.3.0→2.6.0, mistralai 3.6.0→3.7.0, aiobotocore и
  types-aiobotocore(-bedrock-runtime) → 3.10.0, elevenlabs 2.73.0→2.77.0, google-auth 2.62.1 в `uv.lock`.
- **Утягивать:** **НЕ ПРИМЕНИМО** — Python-зависимости.

**`d97433188e`** — [`ci: install just and prek from PyPI instead of GitHub Releases`](https://github.com/ag2ai/ag2/commit/d97433188e)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** CI.
- **Что изменено:** GitHub Releases отдавали 504 на ассеты, из-за чего джобы
  падали до тестов; `just` ставится rust-just-колесом в Python джобы, `prek`
  запускается через `uvx`; в docs-джобе just ставится в venv.
- **Утягивать:** **НЕ ПРИМЕНИМО** — upstream CI.

**`43cbd20561`** — [`fix(mcp): read path-like MCP app documents as UTF-8`](https://github.com/ag2ai/ag2/commit/43cbd20561)
- **Автор:** alexcastgonz (@pcbeingused333). **Вид:** багфикс.
- **Что изменено:** `Path.read_text()` без encoding брал локаль процесса, поэтому
  HTML-документы с не-ASCII ломались на не-UTF-8 локали (Windows cp1252); теперь
  чтение явно UTF-8, BOM отбрасывается, ошибка декодирования отдаётся тем же
  `Could not read MCP app document`, а не голым `UnicodeDecodeError`.
- **Утягивать:** **ПОЗЖЕ** — path-like MCP app documents в AGo не портированы
  (`mcpui` — только конструкторы ресурсов). При порте: читать файл как байты,
  `bytes.TrimPrefix` для BOM и `json.Unmarshal` (он сам требует UTF-8); отдельного
  «locale»-дефекта в Go нет.

**`5125cb5dfc`** — [`fix(tools): preserve wildcard prefixes in recursive file searches`](https://github.com/ag2ai/ag2/commit/5125cb5dfc)
- **Автор:** Ytq1115. **Вид:** багфикс (filesystem glob).
- **Что изменено:** бэкфилл семантики 3.13 в `_glob` разрезал паттерн по первому
  `**`, поэтому паттерны с wildcard в префиксе (например `src*/**/*.py`) теряли
  совпадения; теперь бэкфиллится только хвостовой `**`, остальное отдано
  `Path.glob`. Добавлены тесты.
- **Утягивать:** **НЕТ** — дефекта нет: наш `globWithin`/`matchRecursive`
  (`toolpacks/filesystem.go:295-360`) реализует сегментный матчер 3.13 напрямую,
  без разрезания по `**`, и wildcard-префиксы обрабатывает через `filepath.Match`
  по сегментам. Регрессионные тесты на такие паттерны стоит добавить отдельной
  мелочью, но код менять не нужно.

### Итог синка v1.3.0

- Разобрано **29** коммитов: **17** новых в v1.3.0 + **12** в main после релиза
  (24 более ранних коммита релиза уже разобраны в синке v0.8.0).
- **Утянуть: 10** — `663ddd1574`, `fad570ca19`, `1c27f221a5`, `d716fcbefe`,
  `d503c8ddc8`, `c751dabac4`, `b4814ca6e6`, `3539d07500`, `1b0c399012`,
  `9e7575ae6d`.
- **Отложено: 6** — `d2f9a85dab`, `22a93d852e`, `8db78dd5c3`, `a612a1a39d`,
  `da5dbe1b38`, `43cbd20561`.
- **Не тянем: 2** — `305a50c774` (Gemini Live не портирован), `5125cb5dfc`
  (дефекта нет).
- **Неприменимо: 11** — документы, CI, Python-зависимости и релизная подготовка.

Приоритет ближайшей волны (по соотношению «ценность / риск»): сначала мелкие
провайдерские фиксы — `9e7575ae6d` (стрим без index), `3539d07500` (placeholder
Gemini 3), `d503c8ddc8` (Tavily) и `c751dabac4`+`b4814ca6e6` (extended cache
writes, включая спан-атрибут в `contrib/otelx`); затем wire-правка A2UI
`1b0c399012` отдельной волной с golden-тестами. Крупный блок наблюдаемости
(`663ddd1574` → `fad570ca19` → `1c27f221a5`) и унификация задач `d716fcbefe`
требуют проектной волны: они меняют поверхность `events`/`context`/`agent` и
должны идти с обновлением `docs/specs/core.md`, `docs/DECISIONS.md` и
`docs/PORT_STATUS.md`.

---

## Синк v0.8.0 (2026-11-02/03, PR #11 — влит в main)

Все 24 висящих коммита main после v1.2.2 разобраны. Итог: **12 upstream-коммитов
утянуты** (10 групп работ), 4 служебных неприменимы, 6 разобраны как skip/deferred,
2 отложены до портирования зон. Полный отчёт — в артефакте прогона «Отчёт о релизе
v0.8.0»; коммиты релиза: `ddc845f` (волна 1), `b09565e` (волна 2), `1519940`
(волна 3), `85ba9f0` (волна 4), `f35f9d6` (docs), `da7b54c` (починка ворот),
`6468e5b` (мутационный харнесс `benchmarks/mutation` — постоянный инструмент).

### Взятые коммиты

| Upstream | Наш пункт | Что сделано |
|---|---|---|
| `82bdfb480` | G1 | `WithTasks` и `TaskConfig` помечены `Deprecated:` (godoc/staticcheck), поведение не менялось; AGO-DEVIATION: doc-коммент вместо DeprecationWarning |
| `4d5773e7b` | G1 | Брать нечего: assembly-параметра в AGo никогда не было (middleware-композиция — ядро, D-013); отличие по halt зафиксировано в DECISIONS |
| `17dea422f` + `9031481ec` | G2 | `History.Where`/`HistoryQuery` (терминалы All/First/Last/Count, `SinceLastUserInput`), `RequestOrigin` на ModelRequest с нормализацией, `EnqueueOrigin` в Stream/Context, origin-стампинг в агенте (правило any-user); 19 новых тестов |
| `153ef4cdd` | G3 | Дерево процессов локальных команд убивается при cancel/timeout/close; сентинелы `ErrCommandInterrupted`/`ErrSandboxClosed*`; реестр летящих команд localSandbox |
| `32931b6ef` | G5 | Фикс конкурентных sibling-задач перенесён 1:1 (`prevDep` один слот, `nearestActive`, restore только при владении); тесты писались red-first — 6/8 порядков падали до фикса |
| `be11666a5` | G6 | Сентинел `FinalTurnSkippedToolResult`; executor записывает финальный tool-батч в историю до forced-ответа (реплей ровно один раз, агент- и wire-тесты); docs/specs/tools.md обновлён |
| `15c2a9411` | G9 | `MaxSSEEventSize` в MCPServerConfig (0=дефолт 1 МиБ, >0=порог, <0=безлимит через новый `internal/sse.WithUnlimitedEventSize`); `errors.Is` сквозь `ErrProtocol` |
| `d2274ff2d` | G10 | `automatic_continuation` для gemini (*bool, nil=вкл — паритет SDK; цикл хопов по continuationToken, конкатенация текста, суммирование usage); невидимые SDK-внутренности сознательно не портированы |
| `be31ea6b9` | G11 | Прод-код не менялся (Go os/exec отдаёт сырые байты) — добавлено регрессионное покрытие: хелпер-процесс, fd×exit таблица, фиксация JSON-границы (\ufffd ≡ errors="replace") |
| `6a818894b` | G15 | Прод-код не менялся (вердикт подтверждён чтением) — тесты: запись неудачных вызовов TrackingConfig, изоляция снапшотов |
| `07144b1a8` + `82b15120c` | G14 | Событие `StatusObservation`, проекция model-input (`CopyInputs`/`StatusRequest`/`ObservationRequest`/`ModelInputs`), `middleware/statusbar.go` (StatusCallback + WithPersist), проекция в `agent/turn`, TokenTrigger учитывает события с нулевым весом; D-080/081/082 + контракт в specs/core.md |

### Пропущенные и отложенные

| Upstream | Статус | Причина |
|---|---|---|
| `bd25ea97f` | skip | closeable per-path подписка MemoryKnowledgeStore не портирована (D-027) |
| `78a06eddb` | skip | drive-машина разговорного A2AClient не существует у нас (дефер записан) |
| `59bba8fdd` | skip | extract_skill-путь skill_search не портирован |
| `8fc2c4aee` | already-ok | `events/condition.go` уже без аллокаций (явные циклы, ранний выход); опц. — добавить 2 теста для паритета |
| `d367c8206` | skip | session store (acp) и sessions/conversations (mcpserver) не портированы |
| `c0c7acdc1` | **отложено** | skills script runner не портирован — учесть при портировании (Windows: активный Python) |
| `a21cc7db1` | **отложено** | delete_file/find_files не портированы — учесть symlink-безопасность при портировании |
| `36aa5fde7`, `14c9715ae`, `7430f438b`, `b121a308f` | служебные | Python-deps/docs — неприменимы к Go-порту |

### Проверки релиза

По каждой волне: go build/vet/test корня, независимое ревью (2 оси), property-тесты
(testing/quick), мутационное тестирование (`benchmarks/mutation`) с добивкой выживших.
На релизе: 5 contrib-модулей (vet+test), examples build/vet + smoke
(ask/tools/structured_output). Регрессия, пойманная финальными воротами
(contrib/streamredis пинил представление без Origin), признана устаревшим тестом
(код событий корректен — нормализация Origin совпадает с дефолтом Python-поля);
исправлен seed теста, семантика не менялась (`da7b54c`).

---

## История проверок

- 2026-11-02: завод файла, бейслайн v1.2.2, новых версий нет; зафиксированы 24
  висящих коммита main. Запущен релизный воркфлоу (сверка → волны → синк).
- 2026-11-03: синк v0.8.0 завершён — см. раздел выше. Бейслайн по-прежнему v1.2.2.
- 2026-11-03 (вечер): тег v0.8.0 → GitHub Release опубликован (6 архивов +
  checksums.txt). Первый прогон release-пайплайна поймал два дефекта: (1) probe-тесты
  волны 1 сверяли имя процесса с выводом `pgrep -f`, который печатает только PID-ы —
  ожидание было невыполнимым на всех платформах, а проверки «убит» пустыми; хотфикс
  `32e756c` (predicate probeAlive, windows-probe без `start /b`, бюджет 30с, дамп
  листинга в фаталах); (2) `.mutlogs/` (мусор мутационных агентов) и три gofmt-хвоста,
  маскируемые CRLF, — убраны. Тег переставлен на хотфикс (релиз не успел
  опубликоваться), уроки внесены в пайплайн — пункт (4) в шапке.
- 2026-11-09: проверка нашла новый релиз **v1.3.0** (2026-11-04, тег `1229064a23`).
  Разобраны 17 новых коммитов релиза + 12 коммитов main после него (до
  `5125cb5dfc`); бейслайн переведён на v1.3.0. Итог: 10 «утянуть», 6 отложено,
  2 не тянем, 11 неприменимо — см. раздел «Синк v1.3.0».

