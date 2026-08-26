# workrun

`workrun` — локальный оркестратор агентского workflow для рабочих задач. Он ведёт задачу от чтения tracker item и требований до ветки, проверки, change request, review-цикла и перевода tracker item в `resolved`.

Система специально разделяет ответственность:

- **`workrun`** — единственный владелец состояния, переходов FSM, approvals, leases и audit trail;
- **агентский host** (сначала Qwen Code, также поддержан OMP) — вызывает MCP/native tools и возвращает типизированные evidence;
- **tracker / requirements / source control** — сменные providers, подключаемые через capability bindings;
- **SQLite** — долговечное локальное состояние. `goal` может использоваться только как фокус текущей сессии, но не как persistence.

> Текущий статус: deterministic suite, fake-host end-to-end, CLI smoke, wrappers и release build проходят. Обязательный реальный smoke через Qwen + рабочие MCP bindings ещё нужно выполнить; чек-лист находится ниже.

## Основные гарантии

- Один run = один tracker item + один repository.
- В V1 для одного tracker item разрешён только один активный repository run.
- Активный run закрепляет точные версии workflow и `.agent-workflow.yaml`.
- Код нельзя начинать до hash-bound approval execution brief.
- Review fixes нельзя начинать до hash-bound approval response plan.
- Изменение tracker item или требований инвалидирует approval.
- Remote writes требуют reconciliation и типизированных receipts.
- Финальный publish разрешён только для HEAD, на котором успешно прошла актуальная verification.
- Agent никогда не merge-ит change request, не force-push-ит и не переключает checkout пользователя.
- Human/external waits освобождают lease; side effects выполняет только владелец живого lease.
- MCP credentials, полный stdout/stderr и произвольные provider payloads не попадают в run evidence.

## Архитектура

```text
.agent-workflow.yaml          repository policy
          │
          ▼
Qwen/OMP skill ──typed JSON── workrun CLI ── SQLite
     │                            │
     ├─ MCP tracker               ├─ pinned workflow/config
     ├─ MCP requirements          ├─ FSM + guards
     ├─ MCP source control        ├─ approvals + leases
     ├─ Git/worktree/clone        └─ events + receipts
     └─ one-shot session wake
```

Канонический lifecycle находится в `internal/assets/tracked-change.yaml`:

```text
discovering_context
  → awaiting_brief_approval
  → preparing_workspace
  → implementing
  → verifying ↔ implementing
  → finalizing
  → publishing
  → awaiting_review
  → analyzing_review
  → awaiting_review_plan_approval
  → confirming_review_plan
  → addressing_review
  → awaiting_review
  → merged_pending_resolution
  → resolved
```

Дополнительные состояния: `reconciling`, `brief_stale`, `blocked`, `paused`, `cancelled`, `superseded`, `failed_permanent`.

## Сборка и установка

Требуется Go 1.25+.

```bash
go build -o workrun ./cmd/workrun
install -m 0755 workrun ~/.local/bin/workrun
workrun version
```

Убедись, что `~/.local/bin` присутствует в `PATH`.

Генерация release archives для macOS/Linux, amd64/arm64:

```bash
go run ./cmd/release --version 0.1.0 --out dist
sha256sum -c dist/checksums.txt
```

## 1. Настроить user bindings

User config содержит только capability mapping, trust allowlists, schema hashes, timezone и retention. Credentials остаются внутри Qwen/MCP.

Расположение:

- macOS: `~/Library/Application Support/workrun/config.yaml`;
- Linux: `${XDG_CONFIG_HOME:-~/.config}/workrun/config.yaml`.

`init-user` двухфазный. Первый запуск печатает YAML и approval hash, но ничего не записывает:

```bash
workrun init-user \
  --timezone Europe/Moscow \
  --work-start 09:00 \
  --work-end 18:00 \
  --allow-provider tracker \
  --allow-provider requirements \
  --allow-provider sourcecontrol \
  --allow-provider local \
  --allow-host tracker.example \
  --allow-host requirements.example \
  --allow-host sourcecontrol.example \
  --binding tracker.read=tracker-mcp/read@<64-char-schema-sha256> \
  --binding requirements.read=requirements-mcp/read@<64-char-schema-sha256> \
  --binding tracker.transition=tracker-mcp/transition@<64-char-schema-sha256> \
  --binding tracker.comment=tracker-mcp/comment@<64-char-schema-sha256> \
  --binding change_request.open=sourcecontrol-mcp/open@<64-char-schema-sha256> \
  --binding change_request.poll=sourcecontrol-mcp/poll@<64-char-schema-sha256> \
  --binding change_request.reply=sourcecontrol-mcp/reply@<64-char-schema-sha256>
```

Проверь сгенерированный YAML и повтори **идентичную** команду с hash prefix:

```bash
workrun init-user <те же flags> --approve <hash-prefix>
```

Для замены существующего файла нужны `--force` и новый approval. Минимальная длина hash prefix — 8 символов.

### Capability names

Внешние bindings:

```text
tracker.read
tracker.transition
tracker.comment
requirements.read
change_request.open
change_request.poll
change_request.reply
change_request.resolve_thread   # optional, когда provider умеет resolve
```

Native host capabilities передаются в `agent next`, но обычно не требуют MCP binding:

```text
vcs.branch
vcs.commit
workspace.worktree
workspace.clone
wake.schedule
```

Перед первым использованием в сессии host должен сравнить live tool schema hash с закреплённым `schema_hash`. Drift блокирует действие.

## 2. Создать repository manifest

В каждом repository требуется `.agent-workflow.yaml`. Он описывает только платформо-агностичную policy: target branch, branch template, required requirement roles, bounded traversal, semantic tracker intents и verification/base-update policies.

`init-repo` также двухфазный и требует evidence из авторитетных файлов repository:

```bash
EVIDENCE='{
  "target_branch": "develop",
  "language": "ru",
  "in_review_intent": "in-review",
  "resolved_intent": "resolved",
  "verification_policy": "discover-from-repository",
  "base_update_strategy": "repository-policy",
  "sources": [
    {
      "path": "CONTRIBUTING.md",
      "revision": "<git-blob-or-commit>",
      "finding": "target branch and verification conventions"
    }
  ]
}'

workrun init-repo --evidence "$EVIDENCE" /path/to/repository
```

Проверь proposal и повтори:

```bash
workrun init-repo \
  --evidence "$EVIDENCE" \
  --approve <proposal-hash-prefix> \
  /path/to/repository
```

Manifest запрещает automatic merge и standing force-push override. Tracker transition IDs/names не угадываются: они фиксируются как semantic intent mapping.

## 3. Установить host wrapper

Qwen Code:

```bash
workrun install-agent qwen
```

По умолчанию создаётся `~/.qwen/skills/workrun/SKILL.md` и лениво загружаемые playbooks в `references/`.

OMP smoke adapter:

```bash
workrun install-agent omp
```

Для проверки без записи в host directory:

```bash
workrun install-agent --dest /tmp/workrun-qwen-skills qwen
```

Wrapper тонкий и генерируется из одного canonical source. Он не дублирует весь workflow и читает только playbook текущего action.

## 4. Запустить задачу

```bash
cd /path/to/repository
workrun start --provider tracker --repo . ABC-123
```

Команда возвращает JSON. Сохрани `run.id`:

```text
run_...
```

Повторный `start` для того же tracker item + repository возобновляет активный run. Попытка одновременно запустить этот tracker item для другого repository в V1 блокируется.

Дальше попроси Qwen выполнить run, например:

```text
Продолжи workrun run_... до ближайшего human/external gate.
Показывай brief/review plan и их hash перед запросом approval.
```

Qwen wrapper обязан использовать `workrun` как единственный state authority.

## 5. Human gates

### Execution brief

Agent читает tracker item, description, parent/story и разрешённые relationships, затем собирает нормализованный brief:

- problem;
- scope и non-goals;
- acceptance criteria;
- constraints;
- requirement sources с exact IDs/revisions;
- test strategy;
- explicit overrides;
- отсутствие открытых blocking questions.

Если обязательные business/functional sources не найдены, run блокируется. Агент должен задавать вопросы по одному через `grill-me`, а не выдумывать требования.

После проверки brief:

```bash
workrun approve --by "$USER" run_... <brief-hash-prefix>
```

### Review response plan

Каждый новый actionable batch нормализуется по **thread IDs**. До изменений agent показывает план для каждого thread: что исправить/отклонить, проверки и будущий ответ.

```bash
workrun approve --by "$USER" run_... <review-plan-hash-prefix>
```

Новый overlapping feedback инвалидирует approval. Independent feedback остаётся следующим batch.

## Agent protocol для диагностики

Обычно эти команды вызывает wrapper, а не человек.

### Lease

```bash
workrun agent acquire --owner <session-id> run_...
workrun agent renew --lease <lease-token> --ttl 15m run_...
workrun agent release --lease <lease-token> run_...
```

Вторая сессия не может перехватить живой lease. После expiry новый владелец обязан сначала reconcile незавершённый action.

### Получить action

```bash
workrun agent next \
  --lease <lease-token> \
  --host qwen \
  --cap tracker.read \
  --cap requirements.read \
  --tool-schema tracker.read=<live-schema-sha256> \
  --tool-schema requirements.read=<live-schema-sha256> \
  run_...
```

Ответ содержит:

- `action_id` и одноразовый `nonce`;
- allowed outcomes;
- pinned inputs;
- exact JSON schema required evidence;
- retry class и lease deadline.

### Завершить action

`agent complete` читает один strict JSON document из stdin:

```json
{
  "protocol_version": "1.0",
  "run_id": "run_...",
  "action_id": "action_...",
  "nonce": "nonce_...",
  "lease_token": "lease_...",
  "outcome": "complete",
  "evidence": {},
  "summary": "factual redacted summary"
}
```

```bash
workrun agent complete < result.json
```

Нельзя послать просто `success: true`: evidence валидируется по allowlisted typed schema. Duplicate completion применяется не более одного раза.

Ошибка action:

```bash
workrun agent fail \
  --class transient \
  --reason "provider timeout" \
  < result-identity.json
```

Retryable classes: `transient`, `rate_limited`. Auth, mapping, validation, semantic conflicts и permanent errors блокируют run вместо бесконечного retry.

## Review polling и wakes

SQLite хранит authoritative `next_wake_at`. Qwen создаёт advisory one-shot wake в **текущей** сессии через host cron/tool. После каждого poll планируется следующий wake.

Default intervals задаются workflow и ограничиваются рабочими часами user config. Если session wake потерян:

```bash
workrun status --due --json
workrun resume --due
```

`resume --due` выводит due runs; он не запускает глобальное автоматическое восстановление.

## Операторские команды

```bash
workrun status
workrun status --json
workrun status --history run_...
workrun status --all
workrun status --due

workrun pause --reason "manual investigation" run_...
workrun resume run_...
workrun cancel --reason "task cancelled" --by "$USER" run_...

workrun doctor
workrun export --out /tmp/run-redacted.json run_...
```

`pause` сохраняет workspace/remote artifacts, отменяет wake и освобождает lease. `resume` всегда идёт через reconciliation.

`doctor` проверяет:

- workflow hash/validity;
- SQLite schema и integrity;
- ownership/permissions database и user config;
- repository manifest;
- ожидаемые MCP schema hashes (live probe делает host).

### Diagnostic bundle

Diagnostics требует отдельного approval manifest:

```bash
workrun diagnostics --out /tmp/workrun-diag.json run_...
# проверить manifest/hash
workrun diagnostics \
  --out /tmp/workrun-diag.json \
  --approve <hash-prefix> \
  run_...
```

Bundle содержит redacted run snapshot и event metadata без requirement/review payloads.

## Workflow evolution

Повторяющаяся пользовательская коррекция оформляется proposal, а не тихим изменением installed skill:

```bash
workrun proposal create \
  --run run_... \
  --scope repository \
  --reason "observed correction" \
  --diff '{
    "observed_correction": "...",
    "compatibility_impact": "compatible",
    "tests_required": ["go test ./..."],
    "diff": {"...": "..."}
  }'

workrun proposal list
workrun proposal show proposal_...
workrun proposal approve proposal_... <artifact-hash>
workrun proposal start-change --repo /path/to/workrun proposal_...
```

Изменение самого workflow проходит обычный tracked-change run.

Active runs остаются pinned. Явная миграция сначала показывает preview старого/нового state и gates:

```bash
workrun migrate --workflow /path/to/new-workflow.yaml run_...
workrun migrate \
  --workflow /path/to/new-workflow.yaml \
  --approve <preview-hash-prefix> \
  run_...
```

Pending action мигрировать нельзя.

## Cleanup и retention

Remote branch/change request никогда не удаляются автоматически. Для terminal run можно удалить только local workspace после preview + approval и фактического host evidence:

```bash
workrun cleanup run_...
workrun cleanup \
  --approve <cleanup-hash-prefix> \
  --evidence '{"workspace_removed":true,"reconciled":true}' \
  run_...
```

По умолчанию через 30 дней после terminal state удаляются bulky source/review payloads. Normalized metadata, hashes, receipts и event timeline остаются.

## Реальный Qwen/internal-stack smoke

Это единственный ещё не выполненный acceptance criterion. Для smoke нужна настоящая, безопасная тестовая задача и рабочие Qwen MCP bindings.

### Подготовка

- [ ] Установлен и авторизован Qwen Code.
- [ ] Настроены реальные MCP servers для tracker, requirements и source control.
- [ ] Получены live schema hashes всех используемых tools.
- [ ] Выполнены `workrun init-user`, `workrun init-repo`, `workrun install-agent qwen`.
- [ ] `workrun doctor` показывает safe paths, SQLite `integrity: ok`, schema version `3`.
- [ ] Выбрана disposable tracker task с business/functional requirement references и repository.

### End-to-end сценарий

1. `workrun start` создаёт run в `discovering_context`.
2. Qwen читает tracker item, связи и requirement artifacts через реальные MCP tools.
3. Qwen показывает complete brief; без approval код не меняется.
4. После `workrun approve` создаётся isolated worktree; clone используется только как fallback.
5. Реализация следует brief и repository conventions.
6. Tests/build/lint/smoke проходят на одном HEAD.
7. Commit/push/open change request возвращают typed receipts.
8. Tracker переводится semantic intent `in_review`; один idempotent comment содержит ссылку на change request.
9. Qwen создаёт one-shot wake; последующий poll использует cursor/overlap и deduplication.
10. Добавь реальный actionable review thread.
11. Qwen показывает response plan; до второго approval fixes отсутствуют.
12. После approval Qwen исправляет, проверяет, push-ит, отвечает на каждый covered thread с outcome и commit hash.
13. Merge выполняется человеком/source-control policy, не агентом.
14. Poll наблюдает merge receipt для текущего HEAD.
15. Tracker переводится semantic intent `resolved`; run становится `resolved`.

### Evidence, которое сохранить

```bash
workrun status --history run_... > /tmp/workrun-history.json
workrun doctor > /tmp/workrun-doctor.json
workrun diagnostics --out /tmp/workrun-diagnostics.json run_...
```

Для последней команды сначала выполни preview и approval, как описано выше. Не коммить internal URLs, IDs, payloads или credentials в этот repository.

## Проверка разработки

```bash
go fmt ./...
go test ./...
go test -race ./...
go vet ./...
```

Fake-host E2E покрывает discovery, missing requirements, approvals, drift, workspace fallback, implementation/verification/finalization, publication, adaptive polling, review loop, merge observation, resolution и retry/reconciliation на remote write boundaries.

## Безопасность и troubleshooting

### `unsafe state directory` / `unsafe state database`

State directory должен принадлежать текущему пользователю и иметь mode `0700`; database и user config — `0600`. Symlinks запрещены.

### `active run already exists`

В V1 один tracker item может иметь только один активный repository run. Заверши/cancel текущий run либо используй тот же repository.

### `run lease is held by another owner`

Проверь owner/expiry через `workrun status`. Не удаляй lease из SQLite вручную. Дождись expiry либо попроси текущую сессию вызвать `agent release`.

### `MCP schema drift`

Не обходи проверку. Сверь обновлённую tool schema, field mapping и trust policy; затем пересоздай user config через hash-bound `init-user --force`.

### `requirement drift`

Work item или requirement revision изменилась после approval. Run вернётся в `brief_stale`: перечитай diff, создай новый brief и получи новый approval.

### `waiting until ...`

Run ждёт persisted `next_wake_at`. Проверь Qwen session cron или используй `status --due` после наступления времени.

### SQLite

Не редактируй database вручную. При versioned migration автоматически создаётся mode-`0600` backup. Используй `doctor`, redacted `export` и approved `diagnostics`.

## Подробная спецификация

Полные инварианты, FSM, persistence model, evidence boundary и acceptance criteria находятся в [`SPEC.md`](SPEC.md).
