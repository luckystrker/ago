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

**v1.3.4** — <https://github.com/ag2ai/ag2/releases/tag/v1.3.4> (релиз 2026-11-22,
тег = `db7194e8e26705381d9d23c402ba2bc5cadc67fa`).
Последняя проверка: **2026-11-23**.
Последний просмотренный SHA main: `db7194e8e26705381d9d23c402ba2bc5cadc67fa`
(тег v1.3.4 совпадает с верхушкой main — коммитов после релиза нет).
Предыдущий бейслайн: v1.3.2 (`bf76b9cb66`, 2026-11-15).

> Уточнение к прошлому синку: релиз v1.3.1 **есть**, но его тег называется
> `v.1.3.1` (точка после `v`; опубликован 2026-11-10). Проверку это не меняет:
> бейслайн был и остаётся v1.3.2, v1.3.4 закрывает всё после него.

## Новые версии

**v1.3.4** (2026-11-22) — 20 коммитов после v1.3.3. Почти всё — две волны
безопасности, шедшие в main уже после релиза 1.3.3: права ACP больше не
расширяют «да» до сессионного разрешения (GHSA-3r6w-7pcg-c4x2, ADR 0039), а
shell-`ignore` теперь обходит каталоги-операнды `grep`/`diff`, отказывает
fail-closed и считает единый бюджет файлов. Плюс GHSA-whf5-5qcj-v9m8
(недоверенные ключи словарей больше не читаются как маркеры типа),
`turn_within` для workflow/discussion в network, фикс опроса knowledge после
закрытия подписки и `ACPConfig` как `ModelConfig` (ADR 0040). В `What's Changed`
релиза попали только 5 PR — остальные коммиты пришли merge'ами из форка и в
списке не перечислены.

**v1.3.3** (2026-11-19) — 12 коммитов: поддержка google-genai 3.x (с новым
`api_version` в `GeminiConfig`/`VertexAIConfig`) и deprecation 2.x, завершение
input-транскрипции Gemini Live, чтение обоих полей reasoning в
chat-completions (`reasoning` и `reasoning_content`), фикс ограждений A2UI,
и волна «строгий mypy на весь пакет `ag2`» (allow-list снят).

**v1.3.2** (2026-11-15) — очередной релиз, накрывающий 46 коммитов после v1.3.0
(из них 12 разобраны в синке v1.3.0, 34 новых — ниже). Две главные темы: единая
семантика `Usage.prompt_tokens` (весь вход, включая prompt-cache — ADR 0036,
**breaking** для Anthropic/Bedrock) и ответ на несколько решений в одном запросе
(`DecisionSet`, ADR 0038). Плюс: миграция ACP на SDK 1.0.0, ответ хода на forced-
ответе больше не «съедается» очередью ввода, отдача upstream ext-apps документов
как есть в MCP Apps, строгий mypy по 5 крупным подсистемам, две новые песочницы
в `extensions/` (E2B, boxd). Тега v1.3.1 в релизах не было **на момент релиза
v1.3.2** — версия в `pyproject.toml` бампалась (80c4de8f9c), а опубликован сразу
v1.3.2; позже появился релиз с тегом `v.1.3.1` (см. шапку).

**v1.3.0** (2026-11-04) — новый релиз, первый после бейслайна v1.2.2. Крупный:
наблюдение за деревом делегирования (`ChildEvent`, `origin`), явные области
видимости переменных (`ScopedValue`: COPIED/LOCAL/SHARED), единый жизненный цикл
задач для запусков агента и делегирований, миграция knowledge на плагины,
`cache_creation_extended_input_tokens`, realtime-TTS Mistral, deprecations к 1.4.0
(`TaskConfig`/`Agent(tasks=)`, `Agent(assembly=)`, `Agent(knowledge=)`, `Agent.task()`)
и требование Python 3.11+.

Затем main ушёл на 12 коммитов вперёд (до `5125cb5dfc`, 2026-11-09) — они разобраны
в разделе «main после v1.3.0».

## Синк v1.3.4 (2026-11-23)

**Дата проверки:** 2026-11-23. **Диапазон:** `v1.3.2...v1.3.4` — **32 коммита**:
12 в v1.3.3 (`10bd89e9`…`6d2dd8ba`) и 20 в v1.3.4 (`364c0ce5`…`db7194e8`). Тег
v1.3.4 = `db7194e8e2` = верхушка main, коммитов после релиза нет. Источник:
`curl` + GitHub REST (`/releases?per_page=10`, `/releases/tags/v1.3.3`,
`/releases/tags/v1.3.4`, `/compare/bf76b9cb66...main`), детали диффов — из
локального клона `ag2/` (git-ignored, только чтение).

Две волны безопасности идут в самом конце диапазона и это главное, что стоит
взять: права ACP и `ignore` в shell. Обе задеты у нас, обе воспроизведены
прогоном (временные тесты созданы и удалены, в коммит не попали).

### Безопасность: права ACP — структурированное согласование (ADR 0039)

**`364c0ce5`** — [`fix(acp): let the human pick the agent's permission option, never widen a yes`](https://github.com/ag2ai/ag2/commit/364c0ce5)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс безопасности + правка контракта.
- **Что изменено:** под `permission_policy="ask"` запрос `session/request_permission`
  показывался человеку как yes/no, а любой «да» отвечал **первой** allow-опцией
  агента. Gemini CLI ставит `allow_always` первой, поэтому одно «да» на
  `git status` разрешало все последующие `git`-команды на сессию
  (GHSA-3r6w-7pcg-c4x2). Теперь запрос уходит в `hitl_hook` как
  `ACPPermissionRequest(ToolApprovalRequest)` с опциями агента (`option_id`,
  `name`, `kind`, плюс `kind`/`locations`/`raw_input` самого вызова), а ответ —
  `option_id` выбранной опции, включая отказы. Простые слова не расширяются:
  `yes`/`y`/`1` → one-time allow (или отказ, если такого нет), `always` →
  remembered, всё прочее → отказ; слова читаются **раньше** id, чтобы агент не
  сделал `y` синонимом `allow_always`. `auto`/`deny` предпочитают one-time
  опцию независимо от порядка агента. `ToolApprovalRequest` получил `choices`
  (`ApprovalChoice(value, label)`) — транспортно-нейтральный способ предложить
  больше двух вариантов. Затронуты `acp/permissions.py`, `acp/events.py`,
  `events/types.py`, `ag_ui/interrupts.py`, `context.py`.
- **Утягивать:** **ДА**, дефект у нас тот же. `acp/bridge.go` (`permissionAnswer`):
  политика `Permission func(toolCall, input string) bool` → true, дальше берётся
  первая опция из `allow_once|allow_always` **в порядке агента** — то есть
  `allow_always`, если агент её поставил первой, ровно сценарий Gemini CLI.
  Serve-роль (`acp/serve.go`) этим не больна: она сама синтезирует пару
  `allow_once`/`reject_once`, расширять нечего; дырка — в роли bridge, которая
  отвечает на опции чужого агента. Заодно: `Decision.Allowed()` считает
  разрешением любой выбранный id кроме `"reject"` (значит и `reject_once`);
  сейчас метод живёт только в тестах, но при порте решение надо выводить из
  `kind`, а не из «выбрано и не reject». Объём волны: `Choices` в
  `events.ToolApprovalRequest`, тип `ApprovalChoice`, тип ответа-выбора,
  отрисовка `choices` в схеме ответа `agui`, разбор ответа «слово vs выбор»,
  тесты на оба столкновения (`"y"` как id reject-опции, `"n"` как id
  allow-опции) и запись-аналог ADR 0039 в `docs/DECISIONS.md`.

**`d115d14d`** — [`fix(acp): keep a picked option and a yes/no apart across AG-UI`](https://github.com/ag2ai/ag2/commit/d115d14d)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс безопасности (ревью-раунд той же GHSA).
- **Что изменено:** AG-UI теперь отдаёт ожидающему ходу сырой payload резюма:
  bool → слова `y`/`n` (кнопки approve/refuse), строка, совпавшая со значением
  одной из `choices`, → новый `ApprovalChoiceMessage`. Спрашивающая сторона
  сопоставляет выбор только с id опций, а простое слово — только со словами,
  поэтому кнопка «отказать» не может выбрать allow-опцию с id `"n"`, а выбор
  reject-опции с id `"y"` выбирает именно её. В документации клиента пример
  валидирует номер опции (1..len) вместо `0`, который индексировал последнюю.
- **Утягивать:** **ДА** — часть той же волны и та же слепая зона у нас: в
  `agui/interrupts.go` булев payload превращается в строку и дальше неотличим
  от текста ответа, а `events` не имеет типа «выбор» вообще.

**`47af7402`** — [`Address review on GHSA-3r6w-7pcg-c4x2: the server inferred a pick from a string matching a choice value`](https://github.com/ag2ai/ag2/commit/47af7402)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс безопасности (форма провода).
- **Что изменено:** выбор приходит своим объектом `{"choice": value}`
  (`ChoicePayload`, экспортируется из `ag_ui`); в `responseSchema` это третья
  ветка рядом со `["string","boolean"]`. Строки и bool сохраняют смысл, даже
  если строка равна значению одного из выборов. Невалидный выбор или значение,
  которого не предлагали, отклоняются с `PAYLOAD_REFUSED`. Payload типизирован
  сквозняком: `ResumePayload` (JSON провода) → `ResumeAnswer` (str|bool|Choice).
- **Утягивать:** **ДА** — брать именно в этой форме: `{"choice": ...}` не даёт
  угадывать намерение по строке. У нас `approvalSchema` в `agui/interrupts.go`
  — только string|boolean, а `answerFrom` принимает bool/строку.

**`df85d499`** — [`docs: ADR 0039`](https://github.com/ag2ai/ag2/commit/df85d499)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** документы (правка ADR, 9 строк).
- **Утягивать:** **НЕ ПРИМЕНИМО** — контракт переносим в `docs/DECISIONS.md`
  вместе с кодом волны.

`88bb788d` — «Merge commit from fork»: merge той же волны (16 файлов, +936/−148),
содержимое уже разобрано выше.

### Безопасность: `ignore` в shell — обход дерева, fail-closed, бюджет

**`92e84270`** — [`fix(shell): check files inside grep/diff directory operands against ignore`](https://github.com/ag2ai/ag2/commit/92e84270)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс безопасности (GHSA-3r6w-7pcg-c4x2).
- **Что изменено:** `ignore` сопоставлялся только с литеральными словами
  команды, поэтому в readonly-режиме `grep -r`, `grep` без пути и `diff` по
  каталогам (даже без `-r`) печатали файлы, которые патерны должны скрывать.
  Теперь для `grep|egrep|fgrep|diff` проверяется каждый файл под
  каталогом-операндом (а для grep без файлового операнда — весь workdir), с
  переходом по симлинкам; на удалённых бэкендах, где дерево с хоста не обойти,
  эти программы с `ignore` отклоняются целиком. Разбор опций grep (`-e/-f`,
  длинные имена по префиксу) намеренно ошибается в сторону «меньше
  операндов» — тогда проверяется весь каталог.
- **Утягивать:** **ДА**, дефект воспроизведён. Временный тест в `toolpacks`
  (создан и удалён) на `NewShellTool(WithShellWorkdir(dir), WithShellReadonly(),
  WithShellIgnore(".env"))` дал:
  `grep -r SECRET .` → `./sub/.env:SECRET=2` и `./.env:SECRET=1`;
  `diff -r . sub` → сравнил `.env` и показал секреты. В
  `toolpacks/shellfilter.go` понятия «команда, читающая каталог» нет:
  `checkIgnore` проверяет слова и glob-совпадения одного токена, `checkIgnorePosix`
  — только лексику; блоков `_DIRECTORY_READERS` / `_check_directory_operands`
  нет.

**`c362c5ff`** — [`fix(shell): keep symlink aliases and wildcard directories in the ignore walk`](https://github.com/ag2ai/ag2/commit/c362c5ff)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс безопасности (второй раунд).
- **Что изменено:** каталоги обходятся и под тем именем, которым их назвали
  (у алиаса-симлинка может быть своё правило), и по реальному пути; glob-токен
  раскрывается в неразрешённые пути, чтобы алиас не потерялся; неудачный
  `resolve()` — отказ. Введён единый бюджет `max_entries` (20 000): прогулка
  длиннее — отказ «name a smaller directory». Симлинк-петля в предка не
  раскрывается повторно. В адаптере проверка ушла в worker-thread
  (`asyncio.to_thread`), чтобы обход не блокировал event loop.
- **Утягивать:** **ДА** — в Go блокировка горутины на время обхода приемлема, но
  бюджет, обход «под алиасом и по реальному пути» и отказ при неудачном
  разрешении нужны.

**`3c39c522`** — [`fix(shell): fail closed and bound all work in the ignore check`](https://github.com/ag2ai/ag2/commit/3c39c522)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс безопасности (третий раунд).
- **Что изменено:** всё, что фильтр не может проверить, теперь **отклоняется**:
  путь не разрешается, каталог не листается, встретился `**`, бюджет исчерпан.
  Раньше ошибка `resolve`/`glob` означала «пропустить». Абсолютные wildcard
  внутри workdir проверяются (а не игнорируются), каждый wildcard раскрывается
  один раз, совпадения wildcard и обойдённые записи тратят **один** бюджет.
- **Утягивать:** **ДА** — у нас ошибки как раз проглатываются: `checkIgnore`
  на неудачном `filepath.Abs` возвращает `""` (пропуск), а `ignoreCandidates`
  на ошибке `filepath.Glob` просто не даёт совпадений. Для проверки границы
  безопасности это неверное направление отказа.

**`0c9710c7`** — [`fix(shell): charge every entry a wildcard expansion reads to the ignore budget`](https://github.com/ag2ai/ag2/commit/0c9710c7)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс безопасности (четвёртый раунд).
- **Что изменено:** `Path.glob` читает несовпавшие записи бесплатно, поэтому
  `src/*/*/nothing` мог просканировать всё дерево вне бюджета. Раскрытие идёт
  по сегментам через `scandir`, и **каждая** прочитанная запись тратится из
  бюджета; каталог на пути, который не листается, — отказ.
- **Утягивать:** **ДА** — тот же бюджет, та же поштучная трата; в Go это
  `filepath.Glob` → ручной обход сегментов с `os.ReadDir` и счётчиком.

**`b14405f5`** — [`test(shell): cover the unlistable wildcard directory without patching os.scandir`](https://github.com/ag2ai/ag2/commit/b14405f5)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** тесты.
- **Утягивать:** **НЕ ПРИМЕНИМО** — тест upstream; сценарий (нелистуемый каталог
  на пути wildcard) стоит взять в наши тесты вместе с волной.

**`db7194e8`** — [`docs(tools): stop pairing interpreters with ignore in shell examples`](https://github.com/ag2ai/ag2/commit/db7194e8)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** документы.
- **Что изменено:** из примеров `SandboxShellTool`/`AnthropicBashTool` убраны
  `python`/`uv run` рядом с `ignore`: `ignore` проверяет только литеральные слова
  пути, поэтому `python -c "open('.env')"` проходит. Добавлено предупреждение,
  что `ignore` не защищает от программ, исполняющих код, и поправлено описание
  `blocked` (не остановит `sh -c`/`env`/`xargs`).
- **Утягивать:** **НЕ ПРИМЕНИМО** как код, но у нас та же формулировка в
  описании `WithShellIgnore` («Glob patterns of paths that may not appear in a
  command») — оговорку про интерпретаторы стоит добавить при следующей правке
  доков/описания инструмента (`toolpacks/shell.go`).

`c1d943df` — «Merge commit from fork»: merge волны shell-ignore (6 файлов,
+482/−33), содержимое разобрано выше.

### События: недоверенные ключи словарей больше не читаются как маркеры типа

**`8e8459e4`** — [`fix(events): keep untrusted dict keys from being read as type markers`](https://github.com/ag2ai/ag2/commit/8e8459e4)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс безопасности (GHSA-whf5-5qcj-v9m8).
- **Что изменено:** JSON-формат помечает спецзначения ключами `__dataclass__`/
  `__pydantic__` и доверял этим ключам где угодно. Обычный dict в tool-результате
  с вложенным `__dataclass__` персистился `RedisStream`, и при чтении истории
  загрузчик импортировал названный класс и вызывал его (например `Popen`).
  Теперь: dict с такими ключами заворачивается в `__ag2_dict__` и возвращается
  словарём; `__dataclass__`/`__pydantic__` восстанавливают только dataclass и
  pydantic-модель, иначе `TypeError` **до** вызова; вложенные события и
  `from_dict` отбрасывают неизвестные поля до десериализации.
- **Утягивать:** **НЕТ** — поверхности нет. На проводе AGo один маркер — `@type`
  у события (`events/marshal.go`); вложенные значения десериализуются в
  `map[string]any`/конкретные структуры, `encoding/json` не создаёт типы по
  строке из данных, а неизвестные поля в известном событии игнорируются — это
  ровно вторая половина фикса. `UnknownEvent` хранит сырой map и ничего не
  инстанцирует. Правило на будущее (в DECISIONS при появлении второго маркера):
  полиморфный декодер не доверяет ключам, пришедшим от модели или пира.

**`fd1ece25`** — [`test(stream): drop advisory id from Redis regression test`](https://github.com/ag2ai/ag2/commit/fd1ece25)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** тесты. **Утягивать:** **НЕ ПРИМЕНИМО**.

`e512433d` — «Merge commit from fork»: merge волны сериализации (4 файла,
+121/−18).

### Провайдеры: reasoning в chat-completions

**`7a65080d`** — [`fix(openai): read both reasoning_content and reasoning in completions`](https://github.com/ag2ai/ag2/commit/7a65080d)
- **Автор:** Yibo Liu (@yibol9768-alt). **Вид:** багфикс (OpenAI-совместимые провайдеры).
- **Что изменено:** серверы расходятся в имени поля: DeepSeek, SGLang, xAI,
  DashScope шлют `reasoning_content`; vLLM, OpenRouter, Ollama — `reasoning`.
  Клиент читал `reasoning` только в не-стриме и `reasoning_content` только в
  стриме, поэтому reasoning терялся то там, то там. Добавлен `extract_reasoning`
  (сначала `reasoning_content`, потом `reasoning`), `ModelReasoning` отдаётся
  один раз, даже если сервер заполнил оба поля. Затронуты
  `config/openai/mappers.py`, `openai_client.py`.
- **Утягивать:** **ДА** — та же асимметрия в нашем общем движке
  `config/openaicompat.go`: не-стрим читает `wireChoice.Message.Reasoning`
  (`json:"reasoning"`, стр. 889) и не смотрит `reasoning_content`; стрим читает
  `Delta.ReasoningContent` (`json:"reasoning_content"`, стр. 1069) и не смотрит
  `reasoning`. Правка: один `extractReasoning` для message и delta с приоритетом
  `reasoning_content`, один `ModelReasoning` на ответ, тест на оба диалекта в
  обоих режимах (стрим-тест на `reasoning_content` уже есть).

### Gemini

**`920e6ead`** — [`feat(gemini): support google-genai 3.x and deprecate 2.x`](https://github.com/ag2ai/ag2/commit/920e6ead)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** фича + зависимости (мажор SDK).
- **Что изменено:** диапазон `google-genai` расширен до `>=1.68.0,<4`; 1.4.0
  потребует `>=3`. В 3.x Developer API по умолчанию ходит на `v1`, а не `v1beta`,
  поэтому добавляется `GeminiConfig.api_version` (`"v1"|"v1beta"`, применяется и
  к generation, и к Files API) и `VertexAIConfig.api_version`
  (`"v1"|"v1beta1"`); `http_client` принимает `httpx2.AsyncClient`; на 2.x
  `create()`/`create_files_client()` печатают `DeprecationWarning`; в CI
  отдельная job на `google-genai<3`.
- **Утягивать:** **ПОЗЖЕ (по сути неприменимо сейчас)** — у нас нет SDK:
  `config/gemini` — REST на stdlib, `DefaultBaseURL = .../v1beta`. Наш дефолт
  `v1beta` — надмножество (`v1` не отдаёт `-latest` и часть preview), так что
  ломаться нечему, а версию можно сменить через `WithBaseURL`. Действие только
  на будущее: если появится модель, доступная лишь на `v1`, добавить
  `WithAPIVersion` вместо ручной склейки URL.

**`abb54897`** — [`fix(live): complete Gemini Live input transcription with the whole utterance`](https://github.com/ag2ai/ag2/commit/abb54897)
- **Автор:** Yang Hao (@Assertive-xxh). **Вид:** багфикс (Gemini Live).
- **Что изменено:** при `InputConfig(transcribe=True)` Gemini API не присылает
  `finished`, которого ждал AG2, поэтому `TranscriptionCompletedEvent` для речи
  пользователя не уходил вовсе. Теперь: 3.x-модели присылают финальную
  транскрипцию на высказывание — она завершается как пришла; старые модели шлют
  фрагменты — они склеиваются и завершаются на `finished` либо на конце хода
  модели (`generation_complete`/`interrupted`/`turn_complete`).
- **Утягивать:** **НЕТ** — Gemini Live в AGo не портирован (`live/` — только
  HTTP-каскад OpenAI, D-057), править нечего.

**`f3943efb`** — [`fix(live): complete Gemini 3.5 Live translate input transcriptions as they arrive`](https://github.com/ag2ai/ag2/commit/f3943efb)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс (Gemini Live).
- **Что изменено:** для `gemini-3.5-live-translate-preview` финальная
  транскрипция приходит на каждое высказывание — она завершается сразу, не
  дожидаясь `generation_complete`; тесты переведены на реальную раскладку
  событий.
- **Утягивать:** **НЕТ** — той же причины (Live не портирован).

### A2UI: ограждения вокруг полезной нагрузки

**`e7139c1f`** — [`fix(a2ui): keep single-line fenced JSON payload with a standalone closing fence`](https://github.com/ag2ai/ag2/commit/e7139c1f)
- **Автор:** AILICute (@lkuko). **Вид:** багфикс (парсер A2UI).
- **Что изменено:** `strip_markdown_fences` выбрасывал первую строку всякий раз,
  когда в тексте есть перевод строки, считая её инфо-строкой. Для
  ```` ```json [{"version": "v0.9"}]\n``` ```` полезная нагрузка уходила вместе
  с первой строкой, оставался закрывающий забор, получалась пустая строка,
  `has_a2ui=False` — интерфейс молча не рисовался. Теперь снимается только
  открывающее ограждение (до конца строки или до `{`/`[`) и закрывающее в конце;
  код переписан на две якорные регулярки.
- **Утягивать:** **ДА**, баг есть. `a2ui/parser.go` `StripMarkdownFences`
  (стр. 90) повторяет ту же ветку («есть `\n` → берём всё после первого
  перевода строки»). Проверено временным тестом (создан и удалён):
  ``StripMarkdownFences("```json [{\"a\": 1}]\n```")`` → `""` (нагрузка
  потеряна), а ``"```json\n[{\"a\": 1}]\n```"`` → `[{"a": 1}]`. Правка: снимать
  открывающее ограждение, снимать инфо-строку **только** если остаток —
  закрывающее ограждение (или две якорные регулярки, как upstream), снимать
  закрывающее в конце; в `TestStripMarkdownFences` и `a2ui/golden_test.go`
  добавить оба варианта — с закрывающим забором и без.

### Знания и сеть

**`e3e5c9da`** — [`fix(knowledge): stop polling callbacks after subscription close`](https://github.com/ag2ai/ag2/commit/e3e5c9da)
- **Автор:** Wei Chen (@cw-jlu). **Вид:** багфикс (гонка при закрытии).
- **Что изменено:** `_safe_fire` не проверял закрытость, поэтому колбэк мог
  сработать после `close()` из другой задачи (закрытие во время разноса
  изменений). Добавлен ранний выход при `_closed`. Тест — 147 строк, включая
  закрытие из другой задачи.
- **Утягивать:** **ДА** (низкий приоритет): `knowledge/polling.go` — `safeFire`
  не смотрит `closed`, а `loop` проверяет флаг только перед опросом. При
  `Close()` между опросом и разносом колбэки ещё срабатывают. 3 строки
  (`if w.isClosed() { return }` в `safeFire`) + тест «Close во время разноса не
  вызывает onChange».

**`2aa5bfaf`** — [`fix(network): evaluate turn_within expectations on workflow and discussion channels`](https://github.com/ag2ai/ag2/commit/2aa5bfaf)
- **Автор:** Junxian Lyken Lin (@LykenL). **Вид:** багфикс (ожидания канала).
- **Что изменено:** `WorkflowAdapter`/`DiscussionAdapter` объявляли
  `turn_within`, но в `default_evaluators()` такого оценщика не было — sweeper
  молча пропускал их, и зависший workflow никогда не закрывался; обработчик
  `warn` тоже не был зарегистрирован. Добавлены `TurnWithinEvaluator` (окно от
  конверта, передавшего ход; для первого хода — от `EV_CHANNEL_OPENED`),
  `WarnHandler` (шлёт `EV_EXPECTATION_VIOLATED` только нарушителю),
  `expected_turn` из адаптера в контекст, чтение состояния и WAL под одним
  локом канала, дедуп нарушений с учётом хода (`Violation.turn_key` — один
  участник может сработать на разных ходах) и аудит нарушений, чей
  `on_violation` не имеет обработчика (discussion `hide` — пока только видно).
- **Утягивать:** **ДА** — у нас это выброшено сознательно и зафиксировано
  комментарием, который теперь устарел: `network/adapters.go:485` («Python also
  declares two turn_within expectations; no evaluator exists for that kind»), а
  `WorkflowAdapter` без ожиданий. Что уже есть: `ExpectedTurn` и
  `ExpectedNext()` (`network/turns.go`), три оценщика
  (`acks_within`/`reply_within`/`max_silence`), снимок состояния и WAL под
  локом хаба (`sweepChannel`) — то есть чтение «под одним локом» у нас уже
  сделано. Чего нет: `TurnWithinEvaluator`, `warn`-обработчик, `expectedTurn` в
  `ExpectationContext`, дедуп по `(expectation, violator, turn)` (сейчас ключ
  `idx|kind|violator`, то есть один участник сработает один раз на всю жизнь
  канала). После порта убрать оба `AGO-DEVIATION`-комментария из манифестов.

### Скиллы: токен GitHub в поиске

**`9bdd9d49`** — [`Merge commit from fork: keep the GitHub token out of skill search`](https://github.com/ag2ai/ag2/commit/9bdd9d49)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** багфикс безопасности (утечка креденшела).
- **Что изменено:** `SkillsClient` складывал `Authorization: Bearer <GITHUB_TOKEN>`
  в общие заголовки, поэтому `search()` отправлял токен на
  `https://skills.sh/api/search`. Токен и `Accept: application/vnd.github+json`
  вынесены в отдельный набор, который применяется только `download_skill()`;
  явно заданные `SkillsClientConfig.headers` по-прежнему действуют везде и
  побеждают.
- **Утягивать:** **НЕТ** — поверхности нет: клиента skill-search
  (`tools/skills/skill_search/client.py`) в AGo не портировано, `skills.sh` в
  Go-коде не встречается. Правило на будущее: заголовки аутентификации
  привязаны к хосту, которому они предназначены.

### ACP: `ACPConfig` как `ModelConfig`

**`7a47f7b3`** — [`fix(acp): make ACPConfig a ModelConfig`](https://github.com/ag2ai/ag2/commit/7a47f7b3)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** типы + правка контракта (ADR 0040).
- **Что изменено:** `ACPConfig` не удовлетворял протоколу `ModelConfig`: не было
  `provider` и `create_files_client`, а `model: str | None` конфликтовал с
  `model -> str`. Решение ADR 0040: `ModelConfig.model` теперь `str | None`
  (`None` — «модель выбирает бэкенд», как у ACP-агента), `ACPConfig` —
  наследник `ModelConfig`, `provider` бросает `NotImplementedError`,
  `create_files_client` — `FilesAPIUnsupportedError`, MCP-ответчик сообщает
  `"unknown"` для `None`.
- **Утягивать:** **НЕ ПРИМЕНИМО** — это про несоответствие статическому протоколу
  в mypy: наш `acp.Config` уже реализует `config.ModelConfig`
  (`acp/provider.go`), а «модели нет» в Go выражается пустой строкой без
  конфликта типов. Смысловая часть (читатели имени модели переживают пустое имя)
  у нас выполняется естественно. Запись-аналог ADR 0040 не нужна; в DECISIONS
  достаточно одной строки при случае: «пустая модель у конфига значит, что
  модель выбирает бэкенд».

### Строгий mypy по всему пакету (служебное)

`c81e43f2` (`ag2/network` + тесты моста и ContextVar), `477de5ab` (весь `ag2`,
allow-list снят), `052fb842` (корневые тесты под расслабленным профилем),
`e9a812d3` (`ag_ui`/`events`/`mcp_ui`/observers/policies тесты), `722d1a5b`
(`response` и `eval` тесты). Автор — Semen Frolov (@vvlrff), вид — типы/CI.
- **Утягивать:** **НЕ ПРИМЕНИМО** — это Python-типизация, переносить нечего.
  Полезное побочно: в `c81e43f2` добавлены сценарные тесты
  `test_bridge_drives_workflow.py`, `test_context_vars.py`,
  `test_hub_client_wire_results.py` — их можно взять как идеи для наших
  `network/*_test.go`, когда будем править `turn_within`.

### CI, зависимости, релизы

- `10bd89e9` — dependabot: `github/codeql-action/upload-sarif` 4.39.1→4.39.3.
- `6d2dd8ba` — bump версии до 1.3.3 (+ `uv.lock`).
- `ea7154a5` — bump до 1.3.4 (+ `uv.lock`); это тег v1.3.4.
- **Утягивать:** **НЕ ПРИМЕНИМО** — Go-репозиторий без внешних зависимостей,
  GitHub Actions и Python-версии upstream не используются.

### Итог синка v1.3.4

- Разобрано **32** коммита (12 в v1.3.3 + 20 в v1.3.4), диапазон
  `v1.3.2...v1.3.4`, тег v1.3.4 = верхушка main.
- **Утянуть: 11** — `364c0ce5`, `d115d14d`, `47af7402` (права ACP:
  структурированный выбор опции, выбор отдельно от «да/нет»); `92e84270`,
  `c362c5ff`, `3c39c522`, `0c9710c7` (shell-`ignore`: обход `grep`/`diff`,
  алиасы и wildcard-каталоги, fail-closed, единый бюджет); `7a65080d`
  (reasoning: оба поля в обоих режимах); `e7139c1f` (A2UI: ограждения);
  `2aa5bfaf` (`turn_within` + `warn` в network); `e3e5c9da` (knowledge polling
  после закрытия).
- **Отложено: 1** — `920e6ead` (Gemini `api_version`: у нас нет SDK, `v1beta`
  остаётся надмножеством; вернуться, если появится модель только на `v1`).
- **Не тянем: 4** — `8e8459e4` (маркеров типов на проводе нет, `encoding/json`
  не инстанцирует типы), `9bdd9d49` (клиента skill-search нет), `abb54897` и
  `f3943efb` (Gemini Live не портирован).
- **Неприменимо: 16** — mypy ×5, CI/зависимости/бампы ×3, документы ×2
  (`df85d499`, `db7194e8`), тесты ×2 (`fd1ece25`, `b14405f5`), merge ×3
  (`c1d943df`, `88bb788d`, `e512433d`), типы `ACPConfig` ×1 (`7a47f7b3`).

Приоритет по «вред × вероятность»: (1) **права ACP** — «да» расширяется до
сессионного разрешения, это необратимое действие на машине пользователя;
(2) **shell-`ignore`** — секреты утекают в контекст модели одной командой
`grep -r`; обе волны компактны и уже имеют готовый набор тестов-сценариев в
upstream; (3) `7a65080d` — 10 строк, лечит потерю reasoning; (4) `e7139c1f` —
20 строк, лечит молчащую A2UI; (5) `2aa5bfaf` — средняя волна, но убирает два
устаревших `AGO-DEVIATION`; (6) `e3e5c9da` — три строки, можно брать попутно
с любой волной.

Проверки, снятые при разборе (временные тесты, удалены, в коммит не попали):
`grep -r`/`diff -r` при `WithShellIgnore(".env")` печатают содержимое `.env`;
``StripMarkdownFences("```json [...]\n```")`` возвращает пустую строку.

## Синк v1.3.2 (2026-11-16)

**Дата проверки:** 2026-11-16. **Диапазон:** `v1.3.0...v1.3.2` — 46 коммитов, тег
`bf76b9cb66` = верхушка main (коммитов после релиза нет). 12 коммитов
(`472627dccd`…`5125cb5dfc`) уже разобраны в синке v1.3.0 и повторно не
разбираются; ниже — **34 новых**. Для каждого: id / автор / вид правки / что и
зачем / вердикт «утягивать».

### Usage: единая семантика `prompt_tokens`

**`8464369e40`** — [`fix(usage)!: count prompt-cache tokens in prompt_tokens for Anthropic and Bedrock`](https://github.com/ag2ai/ag2/commit/8464369e40)
- **Автор:** Semen Frolov (@vvlrff).
- **Вид:** багфикс + breaking-правка логики (ADR 0036).
- **Что изменено:** `Usage.prompt_tokens` теперь означает весь вход на любом
  провайдере, включая чтения и записи prompt-cache. `normalize_usage` у Anthropic
  прибавляет `cache_read_input_tokens` + `cache_creation_input_tokens` к
  `input_tokens`, у Bedrock — `cacheReadInputTokens`/`cacheWriteInputTokens` к
  `inputTokens` (и не верит `totalTokens` меньше «весь вход + выход», а
  `cache_*` различает по присутствию, не по истинности — измеренный 0 сохраняется).
  Убрана пер-провайдерная поправка в `ag_ui/usage.py`, ACP-маппер считает так же,
  `TokenMonitor`/`eval TokenUsage`/метрики/телеметрия перестали исключать кэш из
  подсчёта. Затронуты `config/{anthropic,bedrock}/mappers.py`, `ag_ui/usage.py`,
  `acp/mappers.py`, `events/types.py`, `observers/token_monitor.py`, `eval/`.
- **Утягивать:** **ДА**, и в первую очередь — у нас ровно тот же дефект:
  `config/anthropic/mappers.go` (`NormalizeUsage`, стр. 1122) кладёт
  `PromptTokens = InputTokens` без кэша, а `agui/stream.go` держит
  пер-провайдерную поправку `cacheOutsidePrompt = {anthropic, bedrock}`
  (прямо отмечена в `docs/PORT_STATUS.md` как «anthropic/bedrock cache
  correction»). Правка ломает цифры: на кэшированном турне `prompt_tokens`
  вырастет, а значит сдвинутся пороги `TokenMonitor`, суммы `UsageReport` и
  атрибуты спанов в `contrib/otelx`. Нужны: правка `NormalizeUsage`, снятие
  поправки в `agui`, свёртка thinking в `completion_tokens` у батчевого
  `config/gemini` (см. `367119e83a`, D-022), запись в `docs/DECISIONS.md`
  (аналог ADR 0036) и пункт в
  `docs/MIGRATION.md`. Bedrock у нас заглушка (`Create()` → `ErrNotImplemented`),
  его половина — вместе с портом провайдера.

**`367119e83a`** — [`fix(live): include Gemini Live thinking tokens in completion usage`](https://github.com/ag2ai/ag2/commit/367119e83a)
- **Автор:** Yang Hao. **Вид:** багфикс (Gemini Live usage).
- **Что изменено:** Live API отдаёт «мысли» рядом с ответом, из-за чего
  `completion_tokens` их не учитывал; теперь они входят в `completion_tokens`
  (с сохранением breakdown в `thinking_tokens`, как в батчевом Gemini-маппере),
  а `total` меньше «вход + выход» не принимается — та же ADR 0036.
- **Утягивать:** **НЕТ** — Gemini Live в AGo не портирован (`live/` — только
  HTTP-каскад OpenAI, D-057), поверхности для правки нет. Но коммит полезен как
  напоминание: наш **батчевый** `config/gemini` (`normalizeUsage`) кладёт
  `CandidatesTokenCount` в `CompletionTokens`, а `thoughtsTokenCount` — только в
  `ThinkingTokens` (сознательное отступление, D-022), из-за чего у thinking-моделей
  нарушается инвариант `total >= prompt + completion` — ровно то, что чинит ADR
  0036. В волне `8464369e40` это второе место правки: свернуть thinking в
  `completion_tokens` и переписать D-022.

**`fb3061e061`** — [`fix(live): keep Gemini Live thought summaries out of the reply text`](https://github.com/ag2ai/ag2/commit/fb3061e061)
- **Автор:** Yang Hao. **Вид:** багфикс (Gemini Live).
- **Что изменено:** текстовый part с `thought=True` больше не уходит как
  `ModelMessageChunk` и не попадает в `ModelResponse.message` — он публикуется
  как `ModelReasoning`, как в батчевом клиенте; тесты переведены на реальную
  раскладку `gemini-2.5-flash-native-audio`.
- **Утягивать:** **НЕТ** — поверхности нет (Gemini Live не портирован).

### Ход агента

**`3bee17b3c6`** — [`fix(agent): end the turn on a forced response even with input queued`](https://github.com/ag2ai/ag2/commit/3bee17b3c6)
- **Автор:** coder-abu. **Вид:** багфикс (турн-луп).
- **Что изменено:** финальный tool-результат или вызовы, отданные клиентскому
  инструменту, завершают ход `ModelResponse(response_force=True)`;
  `_execute_turn` пропускал для него tool-луп, но затем, как после любого ответа,
  вычитывал inbox — из-за чего отложенный ввод вызывал ещё один вызов модели,
  который перезаписывал финальный ответ (или терял клиентские вызовы), причём
  без ответа на уже отданный вызов в истории. Теперь forced-ответ возвращается до
  вычитки, ввод ждёт следующего хода; докстринги `enqueue`/`spawn_background`
  уточнены, добавлен тест на возобновление клиентского вызова.
- **Утягивать:** **ДА** — тот же дефект в `agent/turn.go` (`turnBody`): после
  tool-лупа блок «leftover mid-turn input gets one extra model call»
  (`agent/turn.go:123`) вычитывает очередь независимо от `resp.ResponseForce`.
  Правка минимальна (ранний
  `return resp` при `ResponseForce` перед вычиткой) + тест «final tool result с
  отложенным вводом: ответ не перезаписан, ввод ждёт следующего хода». Стоит
  проверить и HITL/клиентский путь (`agui` client hand-off), где тот же инвариант.

### Решения (decision-провайдеры)

**`1c3e0c52f8`** — [`feat(response): ask several decision questions in one request`](https://github.com/ag2ai/ag2/commit/1c3e0c52f8)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** фича (ADR 0038, дополняет ADR 0018/0023).
- **Что изменено:** плоский класс в `response_schema` у `TypeSafeConfig` /
  `OpenAIDecisionsConfig` теперь означает «по вопросу на поле» и уходит одним
  запросом (общий вход считается и оплачивается один раз). Новый `DecisionSet` в
  `ag2/response/decision.py`: скаляр — по-прежнему один вопрос `"answer"`;
  вопрос поля берётся из `Annotated[..., Question(...)]`, `Field(description=)`
  или докстринга Enum; вложенные поля, `Optional`, списки, значения по умолчанию,
  пустой класс, вопрос на самом классе и коллизии ключей валидации (алиасы)
  отклоняются до запроса; `metadata` = `{поле: ответ}`, отказ любого поля →
  `DecisionRefusedError(field)`; лимит 400 полей у OpenAI. Затронуты
  `response/`, `config/{openai,typesafe}`, доки.
- **Утягивать:** **ДА** — наш `response/decision.go` умеет только один вопрос
  (`QuestionSchema` + `DecisionSpec.FromResponse` с конвертом `{"data": ...}`), а
  `config/typesafe`/`config/openai` (decisions) на этом построены. В Go
  интроспекция уже есть — `response/schema.go` строит JSON Schema из struct, так
  что «одно поле — один вопрос» реализуется на stdlib: поля struct + теги
  `json`/`description` как вопрос, отказ на вложенных/указателях/срезах/пустом
  struct, `render` собирает `{field: value}` в порядке полей. Отдельная волна с
  ADR-записью (аналог 0038) и тестами на отказы.

### AG-UI

**`27d2dcafcc`** — [`fix(ag-ui): send the reply a model call leaves only in its ModelResponse`](https://github.com/ag2ai/ag2/commit/27d2dcafcc)
- **Автор:** coder-abu. **Вид:** багфикс (AG-UI поток).
- **Что изменено:** текст ответа уходил на провод только из `ModelMessageChunk`
  и `ModelMessage`; но `ToolResult(final=True)` и middleware, отвечающая вместо
  модели, заканчивают вызов одним `ModelResponse` — клиент получал
  `RUN_FINISHED` без текста, хотя `ask()` ответ возвращал. Теперь `ModelResponse`
  отдаёт текст, если вызов не публиковал `ModelMessage`, и закрывает
  отстримленное сообщение; добавлен пер-вызовый флаг `message_sent` (инвариант:
  каждый `ModelMessage` сопровождается `ModelResponse` своего вызова).
- **Утягивать:** **НЕТ** — дефекта нет: в `agui/stream.go` ветка
  `case events.ModelResponse` уже отдаёт текст, когда чанков не было
  (`streamingMsgID == ""` → одиночный `TEXT_MESSAGE_CHUNK`), а события
  `ModelMessage` провайдеры AGo вообще не публикуют (оно живёт только как поле
  `ModelResponse.Message`), так что и двойной отправки нет.

**`056d3e0c23`** — [`fix(ag-ui): reject invalid Base64 characters in inline media`](https://github.com/ag2ai/ag2/commit/056d3e0c23)
- **Автор:** BlueLi. **Вид:** багфикс (валидация входа).
- **Что изменено:** нестрогий `b64decode` молча выбрасывал символы вне алфавита,
  поэтому испорченное значение проходило проверку (и `encryptedValue` вида
  `"aG!k="` переигрывалось как подпись `b"hi"`); теперь декодирование во всех
  четырёх точках идёт с `validate=True`.
- **Утягивать:** **НЕТ** — Go-стандарт уже строгий: `agui/run_input.go:349,358`
  вызывает `base64.StdEncoding.DecodeString`, который отвергает символы вне
  алфавита и битую набивку (паритет с `validate=True`). Единственное расхождение
  — Go игнорирует `\r`/`\n`; оно не даёт «тихого искажения» из примера, поэтому
  достаточно регрессионного теста, код менять не нужно.

**`ddf8c6e3fa`** — [`docs(ag-ui): add assistant-ui quickstart`](https://github.com/ag2ai/ag2/commit/ddf8c6e3fa)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** документы (+ пример).
- **Что изменено:** quickstart «AG2-агент через AG-UI в assistant-ui»: стриминг,
  серверный инструмент, подтверждение tool-call через interrupt, общее состояние;
  ссылка с обзора AG-UI.
- **Утягивать:** **НЕ ПРИМЕНИМО** — документация; сценарии полезны как источник
  тестов для `agui/server.go`, кода не требует.

### ACP

**`3b5bd7d122`** — [`chore(acp): migrate to agent-client-protocol 1.0.0`](https://github.com/ag2ai/ag2/commit/3b5bd7d122)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** зависимости + правка логики (крупная).
- **Что изменено:** пин `agent-client-protocol >=1.0.0,<2`, замена удалённых в 1.0
  SDK-хелперов на схемные модели, новые варианты `SessionUpdate`,
  `EnvVarAuthMethod` убран, новые MCP-методы клиента отвечаются
  `method_not_found`, `httpx2.HTTPError` ловится как транспортная ошибка; новый
  `StdioConnection` для подпроцессов; в `session/new` `cwd` и дополнительные
  каталоги обязаны быть абсолютными; `BridgeState` собирает чанки в целые
  сообщения по `message_id` и закрывает сообщение перед следующим; uri-only
  изображения читаются ссылкой, пустой текст tool-результата не скрывает статус,
  пустое аудио не превращается в файл/вход.
- **Утягивать:** **ПОЗЖЕ** — сама миграция неприменима: у нас нет SDK, `acp/*.go`
  реализован на stdlib (нулевые зависимости — правило репозитория). Поведенческие
  пункты стоит сверить отдельным аудитом, а не переносить коммитом: (1) требование
  абсолютных путей в `session/new` (`acp/serve.go` принимает `CWD` как есть);
  (2) группировка `agent_message_chunk` по `message_id` — у нас границы сообщений
  заданы естественно (один чанк на `ModelMessage`), проверить на клиентской
  стороне `Bridge`; (3) uri-only изображения и пустые блоки в `blockToInput`
  /`blockToFiles`. Взять в волну вместе с ревизией ACP-поверхности.

### MCP Apps

**`3399e3ef2e`** — [`feat(mcp): serve upstream ext-apps App documents as written; mark MCP-UI legacy`](https://github.com/ag2ai/ag2/commit/3399e3ef2e)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** фича + правка логики (ADR 0037).
- **Что изменено:** документ, написанный под `App` из
  `@modelcontextprotocol/ext-apps`, сам делает handshake `ui/initialize`, поэтому
  инъекция ag2ui давала хосту два. Новый `ag2/mcp/_document_scripts.py` (381
  строка) ищет import/`import()`/`src` этого пакета или литерал `"ui/initialize"`
  **только внутри `<script>`** и в таком случае отдаёт тело как есть (упоминание
  в прозе по-прежнему получает ag2ui; пропуск с вызовом ag2ui логируется
  предупреждением). `appInfo.name` стал человеческим (title → явное имя → путь
  URI); `mcp_ui.mdx` переименован в «MCP-UI (legacy embedded resources)».
- **Утягивать:** **ПОЗЖЕ** — MCP Apps UI (SEP-2133) в `mcpserver` не портирован,
  `mcpui` — только конструкторы ресурсов (PORT_STATUS, D-045/D-058). Детектор
  скриптов переносить вместе с Apps-слоем; в Go его роль сводится к разбору HTML
  на `<script>` (stdlib `golang.org/x/net/html` недоступен, значит потоковый
  скан тегов) — это отдельное решение, записать в DECISIONS при порте.

### Расширения: новые песочницы

**`5f471318ad`** — [`feat(extensions): add E2B sandbox integration`](https://github.com/ag2ai/ag2/commit/5f471318ad)
- **Автор:** Maksim Obukhov. **Вид:** фича (новая песочница).
- **Что изменено:** `E2BEnvironment` — `SandboxFactory` над облачными песочницами
  E2B (для `SandboxShellTool`/`SandboxCodeTool`), плюс `E2BSandbox`; создание,
  продление жизни и закрытие сериализованы одним локом на event loop (иначе
  параллельные вызовы сокращали жизнь команды, работали до появления workdir или
  гонялись с close), неудачный kill остаётся повторяемым, таймаут с
  непроверяемым состоянием сообщается как таковой; offline-тесты с мок-SDK, гайд.
- **Утягивать:** **ДА** (по умолчанию для новых фич), низкий приоритет —
  укладывается в наш шов: `toolpacks.SandboxFactory` + `contrib/sandboxdocker` и
  `contrib/sandboxdaytona` (D-113…D-115). E2B — REST поверх HTTP, значит
  отдельный contrib-модуль на stdlib и тесты на `httptest`; проверить на живом
  аккаунте перед продом (как с Daytona). Отметить в PORT_STATUS, что волна
  contrib-песочниц расширяется, а Tenki по-прежнему отложен решением владельца.

**`7c35ee3ab4`** — [`feat(extensions): add boxd sandbox integration`](https://github.com/ag2ai/ag2/commit/7c35ee3ab4)
- **Автор:** MichielMAnalytics. **Вид:** фича (новая песочница).
- **Что изменено:** `ag2.extensions.boxd` — `BoxdEnvironment` (SandboxFactory +
  WorkdirAware) и `BoxdSandbox` по образцу Tenki: песочница = KVM-microVM,
  создаётся лениво, кэшируется по разрешённым параметрам, удаляется на `aclose`
  (плюс `atexit`-фолбэк и серверный `delete_after`); машины изолированы по
  умолчанию (код модели не достаёт до аккаунта boxd), `env_vars` уходят с каждой
  командой, таймаут exec → код 124; правки по итогам ревью — неудачный `open()`
  не закрывает общую песочницу, неудачный delete оставляет машину для `aclose`.
- **Утягивать:** **ДА** (низкий приоритет, та же contrib-волна, что E2B) — API
  песочницы и WorkdirAware у нас уже есть в `toolpacks`.

**`a8195adf81`** — [`fix(types): check ag2/extensions with strict mypy`](https://github.com/ag2ai/ag2/commit/a8195adf81)
- **Автор:** Semen Frolov (@vvlrff). **Вид:** типы + багфикс.
- **Что изменено:** `ag2/extensions` в allow-list mypy, типы NLIP/Exa/mi4afa;
  попутно настоящий багфикс TealTiger: замороженный агент в режиме ENFORCE падал
  с `AttributeError`, теперь получает отказ.
- **Утягивать:** **ПОЗЖЕ** — tealtiger не портирован (PORT_STATUS: «owner has not
  chosen it»), NLIP/mi4afa/Exa-обёртки тоже. Багфикс учесть при порте tealtiger.

### Строгий mypy по подсистемам (служебное)

**`73f859f3d3`**, **`b9f3757325`**, **`d8a8502dbf`**, **`2596cb1aac`** —
strict-mypy по `ag2.eval`, `events/knowledge/streams/middleware/observers/files`,
`ag2.response` + корневые модули, `ag2/agent.py` + `ag2/extensions`. Автор
Semen Frolov (@vvlrff), вид — типы/CI.
- **Утягивать:** **НЕ ПРИМЕНИМО** — это Python-типизация; переносить нечего.
  Проверенные поведенческие детали в Go не воспроизводятся: `upload(str)` →
  `TypeError` невозможен (`files.API.Upload(ctx, data []byte, ...)` типизирован);
  `last_usage`/`Completed.usage` как `dict` (вместо объекта) — деталь pydantic;
  «падение `ResponseSchema` на NamedTuple/tuple/list» — тоже pydantic; `add_tool`
  возвращает `Self` — в Go возврат значения не нужен.

### Документы

`5e7cbc6d9f` (протокол кастомного expectation-evaluator'а в Hub),
`0294b0ae7a` (URL ресурса Arize), `b013c4b101` (Perplexity: совместимость answer
и Sonar EOL), `b371fe35bb` (вложенные поля policy в примере `Rule` + пометка, что
`channel_types`/`rate` hub не применяет), `f8c57aacd7` (`vendor_metadata` для
input-фабрик), `3348cc4af7` (ручное подтверждение приглашения человека),
`d7e91250e0` (проекция `views` по фактическому API), `e027ff3cfa` (примеры
восстановления сети), `54bb6c02b5` (аутентификация Hub и идентичность),
`9c75ef1cd3` (карточка в примере a2a-сервера строится из `url`).
- **Утягивать:** **НЕ ПРИМЕНИМО** — документация и один пример. Полезно как
  сверка при порте network/views и hub-аутентификации, но кода не требует.

### CI, зависимости, релизы

- `e1a63487a9` — bump `astral-sh/setup-uv` 10.3.2→10.3.3 (Actions).
- `10809bf298` — live-тесты Anthropic переведены на Haiku 5.5.
- `4356a5ee64` — Python 3.15 в матрицах тестов и type-check.
- `b799a0f6ba`, `e785b6c67c` — opentelemetry-api 1.45.2, dnspython 2.9.2.
- `80c4de8f9c` — bump версии до 1.3.1 (тега/релиза нет).
- `bf76b9cb66` — bump до 1.3.2 + `uv.lock` (это и есть тег v1.3.2).
- **Утягивать:** **НЕ ПРИМЕНИМО** — Go-репозиторий без внешних зависимостей,
  Python-матрицы и GitHub Actions upstream не используются.

### Итог синка v1.3.2

- Разобрано **34** новых коммита (в диапазоне `v1.3.0...v1.3.2` — 46, из них 12
  уже были разобраны в синке v1.3.0).
- **Утянуть: 5** — `8464369e40` (usage: prompt_tokens включает кэш, breaking),
  `3bee17b3c6` (forced-ответ не вычитывает inbox), `1c3e0c52f8` (`DecisionSet`:
  несколько вопросов одним запросом), `5f471318ad` (E2B), `7c35ee3ab4` (boxd).
- **Отложено: 3** — `3399e3ef2e` (MCP Apps не портированы), `3b5bd7d122`
  (ACP-миграция SDK + аудит поведения), `a8195adf81` (tealtiger не портирован).
- **Не тянем: 4** — `27d2dcafcc`, `056d3e0c23` (дефектов в Go-реализации нет),
  `367119e83a`, `fb3061e061` (Gemini Live не портирован).
- **Неприменимо: 22** — документация, пример, строгий mypy, CI, зависимости,
  релизные бампы.

Приоритет ближайшей волны по «ценность / риск»: (1) `8464369e40` +
`3bee17b3c6` — обе правки маленькие, но первая меняет числа во всех отчётах и
требует записи в DECISIONS/MIGRATION, вторая лечит перезапись финального ответа;
(2) `1c3e0c52f8` — фича среднего размера с чётким контрактом (DecisionSet) и
отказами до запроса; (3) E2B/boxd — отдельная contrib-волна, стартовать после
решения владельца (как с Tenki/Docker/Daytona) и с проверкой на живом аккаунте.
Отложенные MCP Apps и ACP-поведение логично объединить в одну волну «ревизия
serving-поверхности».

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

### Статус портирования (ветка `sync/ag2-v1.3.0`, 2026-11-09)

Все 10 коммитов «утянуть» перенесены — 9 задач, каждая прошла цикл
разработка (DeepSeek V4.1 Flash) → критик с мутационным тестированием → ревью
→ исправления → слияние. T8 и T9 дополнительно приняты независимым
верификатором на другой модели (Muse Spark 1.3).

| Upstream | Задача | Наш коммит | Что сделано |
|---|---|---|---|
| `9e7575ae6d` | T1 | `d0cba08` | tool-call дельты без `index`: `id` открывает новый вызов, иначе продолжается последний |
| `3539d07500` | T2 | `f32ef68` | плейсхолдер `skip_thought_signature_validator` (base64) для неподписанных function-call текущего турна; D-088 |
| `d503c8ddc8` | T3 | `f9e728f` | Tavily `raw_content`/`favicon` + флаги `include_raw_content`/`include_favicon`; регрессия glob `5125cb5dfc` закреплена тестом; починен `TestCodeRunnerUndecodableOutput` под Go 1.27 |
| `c751dabac4` + `b4814ca6e6` | T4 | `f0c6818` | `Usage.CacheCreationExtendedInputTokens` (OpenAI 12h, Anthropic 1h) + атрибут спана в `contrib/otelx`; D-064 |
| `1b0c399012` | T5 | `c29f211` | A2UI: протокол `v1.0` отделён от wire-версии `"1.0"`, спека ре-вендорена, restricted `$ref`; D-089 (wire-break, MIGRATION.md) |
| `663ddd1574` + `a591d84f6e` | T6 | `d23f0f0` | `ChildEvent`/`DownEvent`/`DeliveryFailed`, `SourceEvent`, типизированный маркер доставки, `UsageEvent.EventID`; D-091 |
| `fad570ca19` | T7 | `00db235` | `Scope` COPIED/LOCAL/SHARED, `ScopedValue`, глубокое копирование; **breaking:** `Context.Variables` → `*context.Variables`, `NewWithDeps`/`SetVariable` возвращают ошибку; D-090/090a |
| `1c27f221a5` | T8 | `3540536` | пересылка дерева событий субагентов с провенансом, учёт usage по source-событиям с дедупом по `event_id`, per-stream turn lock с отменой, терминал внутри lock; D-092/092a/092b |
| `d716fcbefe` | T9 | `43b848e` | единый жизненный цикл Task для run и делегации, терминал при любом исходе, тайтл = имя агента, `PARENT_CANCELLED`; D-093/093a/093b |

Сознательно не перенесено (follow-up): downlink из `1c27f221a5` — `StreamHandle`/origin,
адресные `DownEvent`/`HaltEvent`, `DeliveryFailed`-публикация, registry/каскад отмены,
`CANCELLED_TOOL_RESULT` (D-092). Известные мелочи с риском < 5: подписка `Task`, не
доведённого до терминала, остаётся пассивной записью до `Close` (риск 2×2); ветка
`"index": -1` в `config/openaicompat.go` даёт панику (паника была до синка).
Ограничение проверки: `go test -race` в окружении недоступен (нет gcc) — конкурентные
инварианты T7–T9 проверены стресс-прогонами (`-count` до 2000) и чтением кода; нужен
прогон `-race` в CI.

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
- 2026-11-09 (вечер): все 10 коммитов «утянуть» перенесены в ветку `sync/ag2-v1.3.0`
  (9 задач, T1–T9) — см. «Статус портирования». Ветка влита в main, релиз **v0.9.0**.
- 2026-11-16: проверка нашла новый релиз **v1.3.2** (2026-11-15, тег `bf76b9cb66`,
  он же верхушка main — коммитов после релиза нет). Разобраны 34 новых коммита
  диапазона `v1.3.0...v1.3.2` (12 разобраны ранее); бейслайн переведён на v1.3.2.
  Итог: 5 «утянуть», 3 отложено, 4 не тянем, 22 неприменимо — см. раздел
  «Синк v1.3.2». Тега v1.3.1 в релизах нет (бамп версии без публикации).

