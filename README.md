# new_mcp_server — два LLM-агента поверх MCP

Учебный проект на Go: два независимых процесса общаются через
[Model Context Protocol](https://modelcontextprotocol.io/) (MCP) поверх stdio
и совместно играют в «пятнашки» (15-puzzle).

- **agent-a** — MCP-клиент и «генератор» полей.
- **agent-b** — MCP-сервер и «проверяющий», который выносит вердикт о том,
  решено поле или нет.

Оба агента под капотом используют одну и ту же LLM (Mistral через
OpenAI-совместимый API), но играют разные роли.

---

## 1. Архитектура

```
                         ┌───────────────────────────────────┐
                         │            agent-a (клиент)       │
                         │                                   │
                         │   роль: генератор поля 4x4        │
                         │   LLM: Mistral (chat completion)  │
                         │   tools: [check_board]            │
                         └────────────┬──────────────────────┘
                                      │
                         JSON-RPC over stdio (MCP)
                                      │
                         ┌────────────▼──────────────────────┐
                         │            agent-b (сервер)       │
                         │                                   │
                         │   роль: проверяющий                │
                         │   LLM: Mistral (chat completion)  │
                         │   tool: check_board(board)         │
                         └───────────────────────────────────┘
```

- `agent-a` запускается как обычный бинарник.
- `agent-a` запускает дочерний процесс `agent-b` и подключается к нему как
  MCP-клиент по stdio (см. `client.NewStdioMCPClient`).
- `agent-b` поднимает MCP-сервер (`server.ServeStdio`) и регистрирует один
  инструмент — `check_board`.
- В каждом раунде LLM, управляющая `agent-a`, получает в `tools` описание
  `check_board`, генерирует поле и вызывает инструмент, после чего пишет
  финальное резюме.

### Поток одного раунда

1. `agent-a` начинает диалог с системным промптом и сообщением
   «Старт раунда. Действуй.».
2. LLM генерирует поле (строка из 16 токенов через пробел: 15 чисел и `_`).
3. LLM возвращает `tool_calls` с вызовом `check_board(board=...)`.
4. `agent-a` пересылает вызов в MCP-сервер `agent-b`.
5. Внутри `check_board` агент-проверяющий делает свой запрос к Mistral
   с системным промптом «ты — проверяющий 15-puzzle» и `response_format=json_object`,
   получает `{"solved": bool, "reason": "..."}` и отдаёт это обратно.
6. Результат уходит обратно в LLM `agent-a` как сообщение с ролью `tool`.
7. LLM пишет финальное текстовое резюме раунда.

По умолчанию выполняется **3 раунда** (`rounds := 3` в `agent-a/main.go`).

---

## 2. Структура репозитория

```
new_mcp_server/
├── agent-a/
│   ├── main.go        # MCP-клиент + LLM-генератор
│   ├── go.mod
│   ├── go.sum
│   └── agent-a.exe    # сборка под Windows
├── agent-b/
│   ├── main.go        # MCP-сервер + LLM-проверяющий
│   ├── go.mod
│   ├── go.sum
│   └── agent-b.exe    # сборка под Windows
└── README.md
```

Каждый агент — самостоятельный Go-модуль с собственным `go.mod`.

### Зависимости

- [`github.com/mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) `v0.49.0`
  — реализация MCP-клиента и сервера на Go.
- [`github.com/sashabaranov/go-openai`](https://github.com/sashabaranov/go-openai) `v1.41.2`
  — OpenAI-совместимый клиент, через который мы ходим в Mistral.

Go: `1.25.5`.

---

## 3. Что делает каждый агент

### agent-a (`agent-a/main.go`)

Системный промпт `systemPromptA` фиксирует роль и сценарий:

1. Агент сам придумывает поле 4x4 — иногда решённое, иногда нет.
2. **Обязательно** вызывает `check_board` ровно один раз.
3. После ответа от инструмента пишет короткое резюме раунда.

В цикле раунда:

- Из MCP-сервера получаются доступные инструменты (`ListTools`) и
  превращаются в описания функций для OpenAI-совместимого API.
- Идёт цикл «вызов LLM → обработка `tool_calls` → ответ инструмента → следующий
  вызов LLM», ограниченный `maxSteps = 8`.
- Любой `tool_call` с пустым `Type` принудительно переписывается в
  `openai.ToolTypeFunction` (`"function"`) — это нужно, чтобы сообщение
  прошло строгую валидацию Mistral на следующем запросе.
- Поддерживается флаг `checked`: если LLM пытается завершить раунд без
  вызова `check_board`, отправляется до 2 пользовательских напоминаний
  (`remindersLeft = 2`), требующих сделать проверку. Если за весь раунд
  проверка так и не случилась, в лог пишется предупреждение
  `check_board так и не был вызван`.

### agent-b (`agent-b/main.go`)

- Поднимает MCP-сервер `puzzle-checker` v1.0.0.
- Регистрирует один инструмент `check_board` со схемой
  `{ board: string (required) }`.
- Внутри хендлера делает вызов к Mistral с `response_format=json_object`
  и системным промптом `systemPromptChecker`, который требует строго
  валидный JSON `{"solved": bool, "reason": string}`.
- Возвращает результат как `mcp.NewToolResultText(...)`.
- Все логи направляются в stderr (`log.SetOutput(os.Stderr)`), поскольку
  stdout зарезервирован под JSON-RPC сообщения MCP.

---

## 4. Запуск

Никаких CLI-флагов нет — всё конфигурируется через переменные окружения.

### Обязательные переменные

| Переменная | Где читается | Назначение |
|---|---|---|
| `MISTRAL_API_KEY` (или `OPENAI_API_KEY` как fallback) | оба агента | API-ключ Mistral. |
| `AGENT_B_CMD` | только `agent-a` | Полный путь к скомпилированному бинарнику `agent-b`. |

### Опциональные переменные

| Переменная | Default | Назначение |
|---|---|---|
| `MISTRAL_MODEL` (или `OPENAI_MODEL`) | `mistral-small-latest` | Имя модели. |
| `MISTRAL_BASE_URL` (или `OPENAI_BASE_URL`) | `https://api.mistral.ai/v1` | Базовый URL API (для прокси/совместимых эндпоинтов). |

`agent-a` автоматически прокидывает `MISTRAL_API_KEY`, `MISTRAL_MODEL` и
`MISTRAL_BASE_URL` в дочерний процесс `agent-b`, так что отдельно для
`agent-b` их выставлять не нужно.

### Сборка (Windows / PowerShell)

```powershell
cd E:\vscode\new_mcp_server\agent-b
go build -o agent-b.exe .

cd E:\vscode\new_mcp_server\agent-a
go build -o agent-a.exe .
```

### Запуск

```powershell
$env:MISTRAL_API_KEY = "<твой_ключ>"
$env:AGENT_B_CMD     = "E:\vscode\new_mcp_server\agent-b\agent-b.exe"

E:\vscode\new_mcp_server\agent-a\agent-a.exe
```

С кастомной моделью:

```powershell
$env:MISTRAL_API_KEY  = "<твой_ключ>"
$env:MISTRAL_MODEL    = "mistral-large-latest"
$env:AGENT_B_CMD      = "E:\vscode\new_mcp_server\agent-b\agent-b.exe"

E:\vscode\new_mcp_server\agent-a\agent-a.exe
```

---

## 5. Пример вывода

```
2026/04/29 03:34:38 using Mistral endpoint: https://api.mistral.ai/v1 (model=mistral-small-latest)
[agent-b] 2026/04/29 03:34:39 Agent B (MCP server) ready on stdio. provider=mistral model=mistral-small-latest
2026/04/29 03:34:39 Connected to MCP server "puzzle-checker" v1.0.0
2026/04/29 03:34:39 Discovered MCP tool: check_board — Проверяет, решено ли поле игры 'пятнашки' (4x4). ...

=== Раунд 1 ===
Agent A → MCP tool check_board args={"board":"1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 _"}
Agent B (tool result): {"solved":true,"reason":"Поле полностью соответствует решённому виду."}
Agent A: Поле сгенерировано в решённом виде. check_board подтвердил: solved=true. Согласен с вердиктом.
```

---

## 6. История изменений в проекте

Хронология ключевых правок, сделанных по ходу работы.

### Шаг 1. Базовая версия двух агентов

- Создан `agent-b` как MCP-сервер с инструментом `check_board`,
  который внутри ходит в Mistral и возвращает JSON-вердикт.
- Создан `agent-a` как MCP-клиент, который запускает `agent-b` по stdio,
  получает список инструментов, преобразует их в описания функций для
  OpenAI-совместимого API и в цикле раундов даёт LLM генерировать поля
  и опционально звать `check_board`.
- Логи `agent-b` направлены в stderr, потому что stdout занят MCP-протоколом.
- Источник API-ключа, модели и base URL — переменные окружения с поддержкой
  fallback'ов (`MISTRAL_*` → `OPENAI_*`).

### Шаг 2. Обязательная проверка после каждого раунда

Изначально системный промпт давал агенту А свободу: «решай сам — проверять
поле или нет», и в большинстве раундов он эту проверку пропускал. Сделано:

- В `systemPromptA` чётко прописано: шаг 2 — обязательный вызов `check_board`,
  никаких исключений; финальное резюме не пишется до получения ответа от
  инструмента.
- В цикле раунда заведён флаг `checked`. Если LLM пытается завершить раунд
  (вернула ответ без `tool_calls`), но проверка ещё не была сделана — отправляется
  пользовательское сообщение-напоминание и цикл продолжается. Доступно до
  2 таких напоминаний за раунд (`remindersLeft = 2`).
- `maxSteps` поднят с 6 до 8 — чтобы хватило шагов на «черновик → напоминание
  → вызов инструмента → финальное резюме».
- Если за весь раунд проверка так и не произошла, в stderr логируется
  предупреждение.

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
(более строгий, чем у OpenAI) отвергал тело из-за пустого `type`.

Фикс: перед добавлением ответа модели в историю каждому tool-call с пустым
`Type` принудительно проставляется `openai.ToolTypeFunction` (значение
`"function"`).

```169:175:agent-a/main.go
			// Mistral строго требует, чтобы у каждого tool_call поле type было "function".
			// Клиент go-openai иногда оставляет его пустым — выставим явно.
			for i := range msg.ToolCalls {
				if msg.ToolCalls[i].Type == "" {
					msg.ToolCalls[i].Type = openai.ToolTypeFunction
				}
			}
```

---

## 7. Ограничения и идеи на будущее

- Количество раундов зашито константой (`rounds := 3`) — можно вынести в
  переменную окружения или CLI-флаг.
- Системные промпты захардкожены в коде; при желании можно вынести их в
  отдельные файлы или конфиг.
- Сейчас MCP-сервер `agent-b` имеет только один инструмент. Можно добавить,
  например, `solve_board` или `score_board` и научить агента пользоваться
  несколькими инструментами в одном раунде.
- Защитный механизм опирается на здравомыслие LLM: если модель упорно
  отказывается вызывать инструмент даже после двух напоминаний, раунд
  завершается с предупреждением в логах. При желании можно сделать
  принудительный вызов `check_board` со стороны кода, обходя LLM.
