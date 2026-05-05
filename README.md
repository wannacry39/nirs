# new_mcp_server — «пятнашки» через MCP и несколько LLM-агентов

Учебный проект на Go: игра «пятнашки» (15-puzzle), в которой несколько
контейнеров общаются по [Model Context Protocol](https://modelcontextprotocol.io/)
(MCP) через **штатный транспорт Streamable HTTP** ([спецификация MCP](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)),
без самописного REST между компонентами.

Центральный **mcp-server** держит состояние доски в памяти и выступает
роутером: игрок вызывает один MCP-эндпоинт, сервер сам делегирует запросы
агентам **CREATOR** (генерация новой игры) и **CHECKER** (проверка
завершённости и валидность хода). Решение «что вызывать» для текстового
диалога принимает LLM в роли **PLAYER** через обычный tool-calling (список
инструментов приходит с роутера через `tools/list`). Дополнительно пользователь
может отдавать ходы **напрямую** командами вида `/move 5`, минуя рассуждения LLM.

Игровая логика в коде намеренно сведена к минимуму: генерация поля,
проверка хода и «решено ли поле» формулируются в промптах к LLM у CREATOR
и CHECKER; роутер только хранит строку состояния и записывает её после
успешных операций.

---

## 1. Архитектура

```
┌─────────────────────────────────────────────────────────────────────────┐
│  PLAYER (контейнер, AGENT_ROLE=PLAYER)                                   │
│  REPL + LLM (Mistral через OpenAI-совместимый API)                       │
│  MCP-клиент → только http://mcp-server:9000/mcp                         │
│  tools/list → get_state | new_game | is_finished | move                  │
└───────────────────────────────┬─────────────────────────────────────────┘
                                │  MCP Streamable HTTP  (/mcp)
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  mcp-server (:9000)                                                      │
│  • хранилище текущей доски (строка из 16 токенов)                       │
│  • MCP-сервер для игрока                                                 │
│  • пакет clients: MCP-клиенты к CREATOR и CHECKER                       │
└───────┬─────────────────────────────────────────────┬─────────────────┘
        │ MCP /tools/call                               │ MCP /tools/call
        ▼                                                 ▼
┌───────────────────────┐                     ┌───────────────────────────┐
│ CREATOR (:9001/mcp)   │                     │ CHECKER (:9002/mcp)       │
│ AGENT_ROLE=CREATOR    │                     │ AGENT_ROLE=CHECKER      │
│ инструмент:           │                     │ инструменты:             │
│ generate_new_game     │                     │ is_finished              │
│ (LLM → доска+текст)   │                     │ check_is_valid           │
└───────────────────────┘                     └───────────────────────────┘
```

Что важно:

- Транспорт между всеми узлами — **один и тот же**: `Initialize`,
  `tools/list`, `tools/call` поверх Streamable HTTP (`POST /mcp`, при необходимости SSE),
  как в [`mcp-go`](https://github.com/mark3labs/mcp-go).
- **PLAYER** не знает URL CREATOR/CHECKER — только URL роутера.
- **CREATOR** и **CHECKER** не имеют доступа к хранилищу: они получают
  аргументы в вызове инструмента (доску передаёт роутер из своего состояния).

### Поток «новая игра»

1. Игрок (или LLM по запросу пользователя) вызывает инструмент `new_game`.
2. Роутер вызывает у CREATOR MCP-инструмент `generate_new_game`.
3. Ответ парсится (ожидается JSON с полями `board`, `message`), новая доска
   записывается в память роутера.
4. Результат возвращается игроку как текст MCP-результата.

### Поток «ход»

1. Вызывается `move` с номером фишки `tile`.
2. Роутер передаёт в CHECKER текущую доску и `tile` через `check_is_valid`.
3. При успешной валидации (по мнению LLM в CHECKER) роутер сохраняет новую
   доску; ответ уходит игроку.

### Ручные команды в REPL PLAYER

Строки вида `/move 12`, `/state`, `/new тема`, `/done` обрабатываются до
отправки в LLM: выполняется тот же MCP `tools/call` к роутеру, без лишнего
раунда модели. Подсказка при старте и команда `/?` выводят краткий список.

---

## 2. Структура репозитория

```
new_mcp_server/
├── agent-a/
│   ├── main.go          # один бинарь: PLAYER | CREATOR | CHECKER (AGENT_ROLE)
│   ├── go.mod
│   ├── go.sum
│   ├── Dockerfile
│   └── .dockerignore
├── mcp-server/
│   ├── main.go          # роутер + хранилище + HTTP /mcp и /healthz
│   ├── clients/
│   │   ├── mcp.go       # connectMCP, callTool
│   │   ├── creator.go   # CreatorClient → generate_new_game
│   │   └── checker.go   # CheckerClient → is_finished, check_is_valid
│   ├── go.mod
│   ├── go.sum
│   ├── Dockerfile
│   └── .dockerignore
├── docker-compose.yml   # creator, checker, mcp-server (фон) + player (run)
├── .env.example
└── README.md
```

Два Go-модуля: `agent-a` и `mcp-server`.

### Зависимости

- [`github.com/mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) `v0.49.0`
  — MCP-сервер и клиент (Streamable HTTP).
- [`github.com/sashabaranov/go-openai`](https://github.com/sashabaranov/go-openai)
  — OpenAI-совместимый клиент для Mistral и других провайдеров.

Go: **1.25.x** (локально и в `Dockerfile` как `golang:1.25-alpine`).

---

## 3. Что делает каждый компонент

### `mcp-server` (`mcp-server/main.go`)

- Поднимает `server.NewStreamableHTTPServer` на `LISTEN_ADDR` (по умолчанию
  `:9000`), путь `ENDPOINT_PATH` (`/mcp`).
- При старте подключается к CREATOR и CHECKER через пакет `clients`
  (ретраи на время подъёма контейнеров).
- Инструменты **для игрока**: `get_state`, `new_game`, `is_finished`, `move`.
- Делегирование: `new_game` → `CreatorClient.GenerateNewGame`;
  `is_finished` / `move` → `CheckerClient.IsFinished` / `CheckIsValid`.
- `/healthz` — для Docker healthcheck.

### `agent-a` с `AGENT_ROLE=CREATOR`

- MCP-сервер на `LISTEN_ADDR` / `ENDPOINT_PATH` (в compose: `:9001`, `/mcp`).
- Один инструмент: `generate_new_game` — ответ LLM в формате JSON
  (`board`, `message`).

### `agent-a` с `AGENT_ROLE=CHECKER`

- MCP-сервер (в compose: `:9002`, `/mcp`).
- Инструменты: `is_finished`, `check_is_valid` — ответы LLM в JSON.

### `agent-a` с `AGENT_ROLE=PLAYER`

- Подключение к `GAME_SERVER_URL` (роутер MCP).
- `ListTools` → конвертация в `openai.Tool` → цикл chat completion с
  `tool_calls` → `CallTool` на роутер.
- Команды REPL: `/reset`, `/exit`, плюс ручные команды (см. выше).
- Для Mistral: у сообщений с `tool_calls` при необходимости выставляется
  `Type: "function"`, иначе возможна ошибка 422 (как в классическом
  fix для пустого `tool_calls[].type`).

---

## 4. Запуск

### 4.1. Переменные окружения

Общие для всех сервисов с LLM:

| Переменная | Default | Назначение |
|---|---|---|
| `MISTRAL_API_KEY` или `OPENAI_API_KEY` | — | Ключ API. Обязателен. |
| `MISTRAL_MODEL` или `OPENAI_MODEL` | `mistral-small-latest` | Модель. |
| `MISTRAL_BASE_URL` или `OPENAI_BASE_URL` | `https://api.mistral.ai/v1` | Базовый URL. |

**mcp-server:**

| Переменная | Default | Назначение |
|---|---|---|
| `LISTEN_ADDR` | `:9000` | HTTP. |
| `ENDPOINT_PATH` | `/mcp` | Путь MCP. |
| `CREATOR_URL` | `http://creator:9001/mcp` | MCP CREATOR. |
| `CHECKER_URL` | `http://checker:9002/mcp` | MCP CHECKER. |

**creator / checker** (образ `agent-a`):

| Переменная | Назначение |
|---|---|
| `AGENT_ROLE` | `CREATOR` или `CHECKER` |
| `LISTEN_ADDR` | `:9001` / `:9002` |
| `ENDPOINT_PATH` | `/mcp` |

**player:**

| Переменная | Default | Назначение |
|---|---|---|
| `AGENT_ROLE` | — | `PLAYER` |
| `GAME_SERVER_URL` | — | `http://mcp-server:9000/mcp` в compose |

### 4.2. Docker Compose

```bash
cp .env.example .env
# прописать MISTRAL_API_KEY
```

Фоновые сервисы (creator и checker должны стать healthy до старта роутера):

```bash
docker compose up -d --build creator checker mcp-server
```

Интерактивный игрок:

```bash
docker compose run --rm player
```

Остановка: `docker compose down`.

Порты на хосте (для отладки): `9000` — роутер, `9001` — CREATOR, `9002` — CHECKER.

### 4.3. Локальный запуск (без Docker)

Нужны четыре процесса и ключ API. Примеры для PowerShell:

**CREATOR** — из каталога `mcp-server` не нужен; только `agent-a`:

```powershell
$env:MISTRAL_API_KEY="..."
$env:AGENT_ROLE="CREATOR"
$env:LISTEN_ADDR=":9001"
cd agent-a; go run .
```

**CHECKER** — второй терминал:

```powershell
$env:AGENT_ROLE="CHECKER"
$env:LISTEN_ADDR=":9002"
cd agent-a; go run .
```

**Роутер** — третий терминал, каталог `mcp-server`:

```powershell
$env:CREATOR_URL="http://127.0.0.1:9001/mcp"
$env:CHECKER_URL="http://127.0.0.1:9002/mcp"
cd mcp-server; go run .
```

**PLAYER** — четвёртый:

```powershell
$env:MISTRAL_API_KEY="..."
$env:AGENT_ROLE="PLAYER"
$env:GAME_SERVER_URL="http://127.0.0.1:9000/mcp"
cd agent-a; go run .
```

### 4.4. Сборка бинарников

```powershell
cd mcp-server
go build -o mcp-server.exe .

cd ..\agent-a
go build -o agent.exe .
```

---

## 5. Пример сессии (PLAYER)

```
PLAYER готов. /move /state /new /done /? · /reset /exit

you> /state
board {"board":"1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 _"}

you> /new
CREATOR {"board":"…","message":"…"}

you> /move 5
CHECKER {"valid":true,"board":"…","reason":"…"}

you> сделай ещё один ход если можешь
PLAYER> …
```

В логах контейнеров (`docker compose logs`) префиксы вроде `CREATOR …`,
`CHECKER …` показывают, какой агент сформировал ответ на шаге делегирования.

---

## 6. Ограничения и идеи

- Ответы LLM должны оставаться **валидным JSON** там, где клиент роутера
  делает `json.Unmarshal` — при «сломанном» ответе делегирование упадёт
  с ошибкой декодирования.
- Число `tile` в протоколе чаще всего приходит как `float64`; при экзотическом
  типе из другого клиента может понадобиться нормализация в роутере.
- История диалога PLAYER не сохраняется между перезапусками контейнера.
- Для продакшена к `/mcp` имеет смысл добавить TLS и аутентификацию
  (например reverse-proxy с Bearer), как и для любого HTTP-сервиса.

---

## 7. История изменений в проекте

Хронология ключевых правок по ходу работы. Поряд шагов 1–7 отражает
эволюцию **первой** учебной схемы (два агента, `check_board`); шаг 8 —
переход к **текущей** схеме с роутером и отдельными CREATOR/CHECKER.

### Шаг 1. Базовая (учебная) версия двух агентов поверх stdio

- Создан `agent-b` как MCP-сервер с инструментом `check_board`,
  который внутри ходит в Mistral и возвращает JSON-вердикт.
- Создан `agent-a` как MCP-клиент, который **запускал `agent-b`
  как дочерний процесс** и общался с ним по stdio
  (`client.NewStdioMCPClient` + `server.ServeStdio`).
- Логи `agent-b` направлены в stderr, потому что stdout был занят
  MCP-протоколом.
- Источник API-ключа, модели и base URL — переменные окружения с поддержкой
  fallback'ов (`MISTRAL_*` → `OPENAI_*`).

### Шаг 2. Обязательная проверка после каждого раунда (batch-режим)

В исходной batch-версии `agent-a` гонял 3 раунда подряд: «придумай поле →
обязательно вызови `check_board` → напиши резюме». Чтобы LLM не пропускала
шаг проверки, было сделано:

- Системный промпт чётко требовал вызвать `check_board` каждый раунд.
- В цикле раунда заведён флаг `checked`. Если LLM пыталась завершить
  раунд без `tool_calls`, посылалось до 2 пользовательских напоминаний
  (`remindersLeft = 2`).
- `maxSteps` поднят с 6 до 8.
- Если за весь раунд проверка так и не происходила, в stderr логировалось
  предупреждение.

> Вся эта логика была удалена в Шаге 6, когда batch-режим заменили на чат.

### Шаг 3. Фикс ошибки 422 от Mistral (`tool_calls[*].type`)

После шага 2 при многошаговом диалоге начала всплывать ошибка:

```
422 Unprocessable Entity
"loc":["body","messages",2,"assistant","tool_calls",0,"type"]
"msg":"Input should be 'function'", "input":""
```

Причина: клиент `sashabaranov/go-openai` при десериализации ответа Mistral
не всегда заполняет поле `Type` у `ToolCall`. На следующем запросе мы
отправляли это сообщение обратно как часть истории, и валидатор Mistral
отвергал тело из-за пустого `type`.

Фикс: перед добавлением ответа модели в историю каждому tool-call с пустым
`Type` принудительно проставляется `openai.ToolTypeFunction` (`"function"`).

```go
for i := range msg.ToolCalls {
    if msg.ToolCalls[i].Type == "" {
        msg.ToolCalls[i].Type = openai.ToolTypeFunction
    }
}
```

### Шаг 4. Переход на сетевую архитектуру (Streamable HTTP, Docker)

Старая схема, в которой клиент `agent-a` форкал процесс `agent-b` и общался
с ним через stdin/stdout, для прода — антипаттерн:

- два «независимых агента» на самом деле жёстко связаны как родитель-ребёнок;
- никакой сети нет, это IPC через локальные пайпы;
- невозможно вынести агентов на разные хосты;
- невозможно отдельно масштабировать/перезапускать `agent-b`;
- ломается всё, что хочет healthcheck'и, retry, балансировку.

Что сделано:

- **`agent-b`** переведён со `server.ServeStdio` на
  `server.NewStreamableHTTPServer` — это **штатный MCP-транспорт
  Streamable HTTP**, описанный в спецификации MCP, а не самописный REST.
  Слушает `:8080`, эндпоинт `/mcp`. Добавлены `/healthz` и graceful
  shutdown по `SIGINT`/`SIGTERM`.
- **`agent-a`** больше не запускает `agent-b`. Никаких `os/exec`, никаких
  `AGENT_B_CMD`. Подключение идёт через
  `client.NewStreamableHttpClient(AGENT_B_URL)` к настоящему сетевому
  адресу. Перед стартом — короткий polling `/healthz`, чтобы пережить
  момент старта в `docker compose up`. (Полный отказ от polling-ожидания
  и переход на «не страшно если agent-b лежит» сделан в Шаге 7.)
- Добавлены `Dockerfile` для каждого агента (multi-stage,
  `golang:1.25-alpine` → `alpine:3.20`, статический бинарь,
  непривилегированный пользователь, `HEALTHCHECK`).
- Добавлен корневой `docker-compose.yml`: общая сеть `mcpnet`,
  `agent-a` стартовал через `depends_on.condition: service_healthy`
  для `agent-b` (этот связ убран в Шаге 7). `agent-a` ходит к `agent-b`
  по `http://agent-b:8080/mcp` через внутренний docker DNS.
- Добавлены `.env.example`, `.dockerignore`, `.gitignore`.
- Из репозитория удалены пред-собранные `*.exe` Windows-бинарники.

В терминах MCP это **базовый протокол и ничего сверх него**: тот же
JSON-RPC, тот же набор методов (`initialize`, `tools/list`, `tools/call`),
просто транспорт изменился с stdio на штатный HTTP-транспорт MCP.

### Шаг 5. Интерактивный режим (REPL-чаты вместо batch-раундов)

Захардкоженные «3 раунда» — формат демки, а не агента, с которым можно
поговорить. Сделано:

- **`agent-a`** превратился в полноценный REPL-чат: читает stdin,
  держит историю диалога, гоняет цикл «модель → возможные `tool_calls` →
  MCP → модель» на каждое сообщение пользователя. Логика
  `rounds`/`checked`/`remindersLeft` целиком удалена.
- **`agent-b`** научился двум режимам: только MCP-сервер (по умолчанию,
  для фонового запуска в Docker) и MCP-сервер + REPL-чат
  (`AGENT_B_CHAT=1`). HTTP-сервер всегда крутится в goroutine, REPL —
  на основной горутине. Корректное завершение по `SIGINT`/`SIGTERM`.
- В `docker-compose.yml` `agent-a` и отдельный сервис `agent-b-chat`
  переведены под `profiles: ["interactive"]`, добавлены
  `stdin_open: true` / `tty: true`. По умолчанию `compose up` поднимает
  только фоновый `agent-b`; чат — через `compose run --rm agent-a`
  и `compose run --rm agent-b-chat`.

### Шаг 6. Уход от системных промптов: «голые» чаты

В демо-варианте в коде висели три системных промпта: для генератора
(`systemPromptA`), для чекера-чата (`systemPromptB`) и для внутреннего
LLM-вызова в `check_board` (`systemPromptChecker`). Они задавали
«персонажей» и формат ответа.

Запрос был сделать максимально простой pass-through: «что написал
пользователь — то и отправляется в модель, обратно — что вернула».

Что сделано:

- Удалены все три системных промпта из кода.
- В `agent-a` чат стартует с пустой историей; информацию о том, что
  есть инструмент `check_board`, модель получает из штатного механизма
  `tools` (название, описание, JSON Schema аргументов приходят из
  `tools/list` MCP-сервера).
- В `agent-b` чат — плоский pass-through: пользовательское сообщение
  → LLM → ответ.
- В `check_board`-обработчике вместо системного промпта вся инструкция
  сложена в user-сообщение, плюс используется
  `ResponseFormat: {Type: JSONObject}` — это гарантирует валидный JSON
  на выходе средствами API, а не текстом промпта.

В итоге поведение агента-А с инструментом обеспечивается не подсказками
в промпте, а спецификацией tool-calling в OpenAI/Mistral API — через
описание инструмента, которое прозрачно мостится из MCP в `tools` chat
completion.

### Шаг 7. Развязка: agent-a больше не падает, если agent-b недоступен

Изначальная сетевая версия (Шаг 4) предполагала, что `agent-a` обязательно
ждёт готовность `agent-b`: и через `depends_on.condition: service_healthy`
в compose, и через polling `/healthz` (`waitForAgentB`) в коде. Если
`agent-b` не запущен — `agent-a` падал с `log.Fatalf`. Это нарушало
заявленный принцип «два независимых агента».

Сделано:

- В `agent-a` функция `waitForAgentB` удалена. Вместо неё `tryConnectMCP`:
  она делает короткие ретраи `Initialize`/`ListTools` (несколько попыток
  по ~3-5 секунд) и при любой ошибке **возвращает `(nil, nil)`** —
  никакого `log.Fatalf`.
- В `runChat`/`runToolLoop` поддержан режим «без MCP»: если `oaiTools`
  пуст, поле `Tools` в запросе к LLM не выставляется, цикл вырождается
  в одношаговый chat-completion. Стартовое приветствие явно сообщает,
  в каком режиме чат запущен — с инструментом `check_board` или
  standalone без него.
- В `docker-compose.yml` убрана секция
  `depends_on.agent-b.condition: service_healthy` у сервиса `agent-a`.
- Заодно для сервиса `agent-b-chat` (тот же образ, но слушает `:18080`)
  выключен унаследованный из Dockerfile `HEALTHCHECK`
  (`healthcheck: { disable: true }`) — иначе он показывался `unhealthy`
  из-за зашитого порта `:8080` в healthcheck-команде.

Теперь `docker compose run --rm agent-a` без поднятого `agent-b` даёт
рабочий чат с LLM (без инструментов). Если `agent-b` поднять позже —
новый запуск `agent-a` снова обнаружит `check_board` и подключит его.

### Шаг 8. Текущая архитектура: роутер состояния и три роли в agent-a

После шагов 1–7 код эволюционировал в отдельную постановку задачи:

- Введён **`mcp-server`**: хранит строку доски в памяти, поднимает
  публичный MCP для **PLAYER** (`get_state`, `new_game`, `is_finished`,
  `move`). Делегирование к специализированным агентам вынесено в пакет
  **`mcp-server/clients`** (`CreatorClient`, `CheckerClient`) — поверх тех же
  MCP-вызовов к другим HTTP-эндпоинтам.
- **`agent-b`** как отдельный модуль удалён; один репозиторий **`agent-a`**
  собирается с `AGENT_ROLE=CREATOR | CHECKER | PLAYER`.
- **CREATOR** и **CHECKER** — полноценные MCP-серверы на своих портах;
  инструменты `generate_new_game`, `is_finished`, `check_is_valid`; игровая
  логика в промптах к LLM по заданию учебного проекта.
- **PLAYER** подключается только к роутеру, получает `tools/list` с него,
  при необходимости исправляет пустой `type` у tool calls (наследие Шага 3).
- Добавлены **ручные команды** в REPL (`/move`, `/state`, `/new`, `/done`, …)
  без лишнего раунда LLM.
- **Docker Compose**: сервисы `creator`, `checker`, `mcp-server`, профиль
  `interactive` для `player`; `depends_on` с `service_healthy` для корректного
  порядка старта.
- Документация и комментарии в compose приведены в соответствие с MCP-между
  всеми сервисами (без устаревших отсылок к «чистому HTTP» между агентами).

Исторические имена **`agent-b`**, **`check_board`**, порт **`:8080`** относятся
к шагам 1–7; в актуальном коде их заменяют сервисы **creator/checker** и
перечисленные выше инструменты.
