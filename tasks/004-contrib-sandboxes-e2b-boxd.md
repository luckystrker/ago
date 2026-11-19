---
id: 004
title: contrib/sandboxe2b и contrib/sandboxboxd — песочницы E2B и boxd на шве toolpacks.SandboxFactory
status: blocked
priority: 3
mode: parallel
depends_on: []
wave: C
human: required
upstream: 5f471318ad, 7c35ee3ab4
decisions: D-129, D-130
executor_model: claude-sonnet-5-5
verifier_model: opencode-go/deepseek-v4.1-flash
---
> **blocked:** ждёт решения владельца о старте волны (E2B/boxd новые, он их не выбирал; Docker/Daytona — его выбор
> в волне 3, Tenki им отложен). **human: required** — живая проверка требует аккаунтов и ключей E2B/boxd (доступы и деньги);
> ключи владелец передаёт через bb secrets (dotenv, агент значений не видит); в `done` переводит только он.

## Что сделать
Два независимых модуля **по образцу `contrib/sandboxdaytona`** (свой `go.mod`: `module github.com/luckystrker/ago/contrib/<имя>`,
`go 1.24`, `replace github.com/luckystrker/ago => ../..`); CI сам обходит `contrib/*/`. Оба реализуют `toolpacks.SandboxFactory`
и `toolpacks.WorkdirAware` (`toolpacks/sandbox.go` ~133/141). Сначала E2B полностью, потом boxd.

**E2B** (`git -C <ag2> show 5f471318ad`): `E2BEnvironment` + `E2BSandbox`. Первым шагом выясни по исходникам SDK/OpenAPI, как
ходят exec и файлы (envd; Connect-RPC умеет JSON поверх HTTP/1.1). Создание, продление жизни и закрытие — под одним мьютексом
(иначе параллельные вызовы сокращали жизнь, работали до появления workdir, гонялись с close); неудачный kill повторяем;
таймаут с непроверяемым состоянием — отдельный сентинел (`ErrStateUnknown`), не «успех» и не «провал».

**boxd** (`git -C <ag2> show 7c35ee3ab4`): песочница создаётся лениво, кэшируется по разрешённым параметрам, удаляется на `Close`,
серверный `delete_after` передаётся; Python-фолбэк `atexit` в Go не воспроизводим → `// AGO-DEVIATION` (Close + `delete_after`);
изоляция по умолчанию; `env_vars` — с **каждой** командой; таймаут exec → 124; неудачный `open()` не закрывает общую песочницу;
неудачный delete оставляет машину для следующего `Close`.

Общее: только stdlib HTTP — если протокол требует protobuf/gRPC/SDK-зависимость, **стоп**, `board msg teamlead`.
Ключи API — из опций/окружения как в Python, в логах и ошибках не печатать. Документы: `docs/DECISIONS.md` **D-129** (E2B),
**D-130** (boxd) по образцу D-115; `docs/PORT_STATUS.md` — строки `extensions/e2b/`, `extensions/boxd/`, общая строка `extensions/`
(Tenki по-прежнему отложен); `docs/MIGRATION.md` §11 — «новые опциональные модули».

## Финишная черта тикета
- [ ] Офлайн-тесты на `httptest`-фейках. E2B: create → exec (stdout/stderr/код) → upload/download → close; повторный close; неудачный kill → повтор успешен; таймаут → `ErrStateUnknown`; N горутин exec+extend+close — фейк фиксирует один create и close после всех exec. boxd: 0 вызовов до первой команды; те же параметры → 1 create, другие → 2; env в каждом exec; таймаут → 124; неудачный open не трогает общую; неудачный delete → повторный Close удаляет. — артефакт: вывод `go test -count=1 -v ./...` в каждом модуле.
- [ ] **Р (офлайн, Г для слияния кода):** программа в `$BB_THREAD_STORAGE`: агент с `toolpacks.WithShellSandbox(<фабрика>)` против фейка выполняет `echo hi` с `env`, печатает журнал HTTP-вызовов фейка. — артефакт: вывод для каждого провайдера.
- [ ] **Р (живой, нужен владелец):** тот же сценарий против настоящих API (create, `echo hi`, файл туда-обратно, env, таймаут, close) + доказательство удаления машины (ответ API «not found» / пустой список). Лог с замаскированными ключами — в отчёт; в PORT_STATUS «проверено на живом аккаунте <дата>». До этого строки PORT_STATUS — «не проверено на живом аккаунте», как у Daytona.

## Не трогать
- Корневые пакеты, включая `toolpacks/` (шов не меняется; нужна правка — к тимлиду), другие `contrib/*`, `.github/`. Тестов с сетью в репозиторий не добавлять.

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
