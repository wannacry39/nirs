# new_mcp_server — два LLM-чат-агента поверх MCP

Учебный проект на Go: два независимых сервиса в Docker-контейнерах,
каждый из которых — обычный чат с LLM. Между собой они общаются
по [Model Context Protocol](https://modelcontextprotocol.io/) (MCP)
через **штатный транспорт Streamable HTTP** (часть базовой спецификации
MCP, не самописный REST).

- **agent-a** — чат, у которого опционально подключён MCP-инструмент
  `check_board`. Если `agent-b` доступен — попроси проверить поле
  «пятнашек», и он сходит к `agent-b` по MCP, получит вердикт и
  пересскажет пользователю. Если `agent-b` лежит — `agent-a` просто
  работает как обычный чат с LLM, без инструментов.
- **agent-b** — чат + MCP-сервер с инструментом `check_board`.
  С пользователем общается напрямую как обычная LLM, а инструмент
  отвечает на запросы от `agent-a`.

Агенты намеренно НЕ связаны жёстко: каждый стартует независимо,
работоспособность одного не зависит от другого.

Никаких системных промптов: оба чата — это «голый» pass-through
к модели (что написал пользователь, то и улетело в LLM, обратно
вернулся ответ). Поведение агента А с инструментом обеспечивается
не системным промптом, а штатным механизмом tool-calling: модель
сама читает описание `check_board` (полученное от MCP-сервера через
`tools/list`) и решает, нужно ли его вызвать.

---

## 1. Архитектура

```
                 ┌──────────────────────────────┐
                 │           agent-a            │
                 │  (контейнер, чат-REPL)       │
                 │                              │
                 │  голый чат с LLM (Mistral)   │
                 │  tools: [check_board]        │
                 │  MCP-клиент (Streamable HTTP)│
                 └────────────┬─────────────────┘
                              │
                              │  MCP / JSON-RPC
                              │  поверх HTTP (POST /mcp + SSE)
                              │  → стандартный транспорт MCP,
                              │    не самописный REST-API
                              ▼
                 ┌──────────────────────────────┐
                 │           agent-b            │
                 │  (контейнер)                 │
                 │                              │
                 │  • MCP-сервер на :8080/mcp   │
                 │      инструмент check_board  │
                 │      (Streamable HTTP)       │
                 │  • опционально: чат-REPL     │
                 │      (AGENT_B_CHAT=1)        │
                 └──────────────────────────────┘
```

Что важно:

- `agent-b` поднимает `server.NewStreamableHTTPServer` из
  [`mcp-go`](https://github.com/mark3labs/mcp-go) и слушает порт `:8080`,
  эндпоинт `/mcp` (это **базовый MCP-транспорт**, см.
  [спецификацию MCP](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)).
- `agent-a` подключается к `agent-b` через `client.NewStreamableHttpClient`
  по URL из переменной окружения `AGENT_B_URL` (по умолчанию
  `http://localhost:8080/mcp`).
- Никаких stdio, никаких дочерних процессов, никаких самописных
  HTTP-эндпоинтов поверх MCP.

### Поток одного сообщения «проверь поле»

1. Пользователь в чате `agent-a` пишет: *«сгенерируй поле и проверь, выигрышное ли оно»*.
2. `agent-a` отправляет это сообщение в Mistral вместе со списком доступных
   `tools` (туда был автоматически конвертирован `check_board` из
   `tools/list` MCP-сервера `agent-b`).
3. LLM отвечает с `tool_calls`: `check_board(board="...")`.
4. `agent-a` пересылает вызов в MCP-сервер `agent-b` по HTTP
   (`POST /mcp`, JSON-RPC `tools/call`).
5. Хендлер `check_board` в `agent-b` делает свой запрос к Mistral
   (с инструкцией прямо в user-сообщении и `response_format=json_object`),
   получает `{"solved": bool, "reason": "..."}` и отдаёт это обратно
   как MCP-результат инструмента.
6. Результат возвращается в LLM `agent-a` как сообщение с ролью `tool`.
7. LLM формирует обычный человеческий ответ для пользователя.

Если пользователь просто болтает («привет», «как дела», «что за пятнашки?») —
никакой `check_board` не вызывается: модель сама определяет, нужен ли
инструмент в текущем шаге.

### Поток обычного сообщения в `agent-b` чате

1. Пользователь пишет в REPL `agent-b` любое сообщение.
2. `agent-b` отправляет это сообщение (плюс прежнюю историю диалога,
   без какого-либо системного промпта) в Mistral.
3. Получает ответ модели и печатает его в stdout.

---

## 2. Структура репозитория

```
new_mcp_server/
├── agent-a/
│   ├── main.go         # MCP-клиент (Streamable HTTP) + chat REPL с tool-calling
│   ├── go.mod
│   ├── go.sum
│   ├── Dockerfile
│   └── .dockerignore
├── agent-b/
│   ├── main.go         # MCP-сервер (Streamable HTTP) + опциональный chat REPL
│   ├── go.mod
│   ├── go.sum
│   ├── Dockerfile
│   └── .dockerignore
├── docker-compose.yml  # agent-b (фон) + agent-a / agent-b-chat (interactive)
├── .env.example        # шаблон переменных окружения
└── README.md
```

Каждый агент — самостоятельный Go-модуль с собственным `go.mod`.

### Зависимости

- [`github.com/mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) `v0.49.0`
  — реализация MCP-клиента и сервера на Go (включая Streamable HTTP transport).
- [`github.com/sashabaranov/go-openai`](https://github.com/sashabaranov/go-openai) `v1.41.2`
  — OpenAI-совместимый клиент, через который мы ходим в Mistral.

Go: `1.25` (для локальной сборки), либо `golang:1.25-alpine` в Docker
(см. `Dockerfile`-ы).

---

## 3. Что делает каждый агент

### agent-a (`agent-a/main.go`) — chat + MCP-клиент

- На старте: подключается к `agent-b` (Streamable HTTP), делает
  `Initialize`, тянет `tools/list` и конвертирует MCP-инструменты
  в формат `openai.Tool` для chat completion.
- Запускает REPL: читает stdin, добавляет ввод в историю как `user`,
  гоняет цикл «модель → возможные `tool_calls` → MCP → модель»,
  печатает финальный ответ в stdout.
- Системного промпта нет. Решение «вызывать ли `check_board`» модель
  принимает сама на основе описания инструмента, которое пришло
  из MCP-сервера.
- Команды: `/reset` — очистить историю, `/exit` или Ctrl+D — выход.

### agent-b (`agent-b/main.go`) — MCP-сервер + (опционально) chat

- Поднимает MCP-сервер `puzzle-checker` v1.0.0 на транспорте
  Streamable HTTP (`server.NewStreamableHTTPServer`).
- Слушает `AGENT_B_LISTEN_ADDR` (по умолчанию `:8080`), эндпоинт
  `AGENT_B_ENDPOINT_PATH` (по умолчанию `/mcp`).
- Регистрирует один инструмент `check_board(board: string)`.
- Внутри `check_board`: запрос к Mistral с инструкцией прямо в
  user-сообщении (без системного промпта) и `response_format=json_object`.
  Парсит `{"solved": bool, "reason": string}` и возвращает как MCP-результат.
- Если задан `AGENT_B_CHAT=1`, **дополнительно** запускается REPL-чат
  на stdin/stdout — тоже без системного промпта, плоский pass-through
  к модели. MCP-сервер в этот момент крутится в фоне.
- Отдаёт `/healthz` для healthcheck'а Docker / k8s.
- Корректно завершается по `SIGINT`/`SIGTERM` (graceful shutdown).

---

## 4. Запуск

### 4.1. Переменные окружения

Общие (нужны в обоих контейнерах):

| Переменная | Default | Назначение |
|---|---|---|
| `MISTRAL_API_KEY` (или `OPENAI_API_KEY` как fallback) | — | API-ключ Mistral. Обязателен. |
| `MISTRAL_MODEL` (или `OPENAI_MODEL`) | `mistral-small-latest` | Имя модели. |
| `MISTRAL_BASE_URL` (или `OPENAI_BASE_URL`) | `https://api.mistral.ai/v1` | Базовый URL API. |

Только для `agent-a`:

| Переменная | Default | Назначение |
|---|---|---|
| `AGENT_B_URL` | `http://localhost:8080/mcp` | URL MCP-эндпоинта `agent-b`. В docker-compose выставляется в `http://agent-b:8080/mcp`. |

Только для `agent-b`:

| Переменная | Default | Назначение |
|---|---|---|
| `AGENT_B_LISTEN_ADDR` | `:8080` | Адрес HTTP-листенера. |
| `AGENT_B_ENDPOINT_PATH` | `/mcp` | Путь MCP-эндпоинта. |
| `AGENT_B_CHAT` | (не задан) | Если `1` — запустить REPL-чат на stdin вместе с MCP-сервером. |

### 4.2. Запуск через Docker Compose (рекомендуется)

Перед стартом:

```bash
cp .env.example .env
# отредактируй .env: MISTRAL_API_KEY=...
```

Поднимаем фоновый MCP-сервер `agent-b`:

```bash
docker compose up -d agent-b
```

Чтобы пообщаться с `agent-a` (тут можно просить «придумай поле и проверь его»):

```bash
docker compose run --rm agent-a
```

Чтобы пообщаться с `agent-b` напрямую (отдельный контейнер с тем же
образом, в режиме REPL-чата):

```bash
docker compose run --rm agent-b-chat
```

Можно запустить оба чата одновременно — в разных терминалах. Между
собой `agent-a` и фоновый `agent-b` общаются по docker-сети `mcpnet`
(имя `agent-b` резолвится во внутренний IP).

Остановить всё: `docker compose down`.

> Порт `8080:8080` пробрасывается на хост только для отладки. Для связи
> между контейнерами он не нужен — `agent-a` ходит по
> `http://agent-b:8080/mcp` через внутренний docker DNS.

### 4.3. Локальный запуск без Docker

В разных терминалах.

**Terminal 1 — фоновый MCP-сервер `agent-b`** (Windows / PowerShell):

```powershell
$env:MISTRAL_API_KEY     = "<твой_ключ>"
$env:AGENT_B_LISTEN_ADDR = ":8080"

cd E:\vscode\new_mcp_server\agent-b
go run .
```

**Terminal 2 — чат с `agent-a`**:

```powershell
$env:MISTRAL_API_KEY = "<твой_ключ>"
$env:AGENT_B_URL     = "http://localhost:8080/mcp"

cd E:\vscode\new_mcp_server\agent-a
go run .
```

**(опционально) Terminal 3 — чат с `agent-b` напрямую**:

```powershell
$env:MISTRAL_API_KEY     = "<твой_ключ>"
$env:AGENT_B_CHAT        = "1"
$env:AGENT_B_LISTEN_ADDR = ":18080"   # чтобы не конфликтовать с Terminal 1

cd E:\vscode\new_mcp_server\agent-b
go run .
```

Аналогично для Linux/macOS, заменив `$env:NAME = "..."` на `export NAME=...`.

### 4.4. Сборка бинарников вручную

```powershell
cd E:\vscode\new_mcp_server\agent-b
go build -o agent-b.exe .

cd E:\vscode\new_mcp_server\agent-a
go build -o agent-a.exe .
```

---

## 5. Пример сессии

В `agent-a`-чате:

```
agent-a: чат готов. Просто пиши, что хочешь.
         (если попросишь проверить поле «пятнашек» — схожу к agent-b по MCP)
         /reset — очистить историю, /exit или Ctrl+D — выйти.

you> привет, кто ты?
agent-a> Привет! Я ассистент с подключённым инструментом для проверки полей «пятнашек». Спрашивай.

you> сгенерируй поле и проверь, решено ли оно
agent-a> Сгенерировал поле: 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 _.
         Проверка через check_board подтвердила: solved=true — поле уже в решённом виде.

you> /exit
```

В этот момент в `agent-b` (фон) в stderr будет:

```
[agent-b] check_board called: board="1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 _"
[agent-b] check_board verdict: solved=true reason="Поле полностью соответствует решённому виду."
```

В отдельном `agent-b`-чате:

```
agent-b: чат готов. Просто пиши, что хочешь.
         /reset — очистить историю, /exit или Ctrl+D — выйти.

you> что такое «пятнашки»?
agent-b> Это головоломка 4×4: пятнадцать фишек 1..15 и одна пустая клетка...
```

---

## 6. История изменений в проекте

Хронология ключевых правок, сделанных по ходу работы.

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

---

## 7. Ограничения и идеи на будущее

- Системных промптов нет — поведение модели целиком зависит от
  описания MCP-инструментов и здравомыслия LLM. Если модель почему-то
  начнёт игнорировать `check_board` — проще не возвращать промпт, а
  улучшить `description` инструмента в `agent-b`.
- Сейчас MCP-сервер `agent-b` имеет только один инструмент. Можно
  добавить, например, `solve_board` или `score_board` и научить агента
  пользоваться несколькими инструментами в одном диалоге.
- Сейчас `agent-b` слушает голый HTTP. Для прод-сценария стоит добавить
  TLS (либо терминирование на reverse-proxy типа nginx/Traefik, либо
  через `WithTLSCert` опцию `mcp-go`) и аутентификацию (например,
  Bearer-токен через middleware на `/mcp`).
- В чатах нет персистентности истории: после `docker compose down`
  диалог исчезает. Для учебного проекта это норма; для прода стоило
  бы вынести `messages` в стораж (sqlite/redis) и восстанавливать
  по идентификатору сессии.
- `agent-a` определяет доступность `agent-b` только при старте.
  Если `agent-b` поднять уже после старта `agent-a`, чат продолжит
  работать как plain (без `check_board`). Можно добавить либо
  команду `/reconnect`, либо фоновый retry с переоткрытием MCP-клиента,
  если он лёг во время сессии.
