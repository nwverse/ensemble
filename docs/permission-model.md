# Ensemble 权限模型（5 级助手指挥、FollowLease 与安全护栏）

本文档说明 `nwverse/ensemble`（`personal-agent-club`）的 5 级助手指挥权限模型（`assistant_control`）、按对话持续跟进租约（`FollowLease`）、`auto`（直接放行 / Bypass）安全护栏，以及与 Persona Hub 门禁的分工与安全边界。

---

## 一、5 级权限模型（`assistant_control`）

每个已配对账号独立维护 `assistant_control` 等级（定义在 `internal/core/control.go`）。新配对账号与通过导入包导入的账号默认均为 **`off`**。

| 等级 (`Control`) | UI 名称（中文 / 英文） | 允许的操作 (`OpList` / `OpRead`) | 写操作 (`OpSend` / `OpStart` / `OpInterrupt`) | 后台 / 无人值守 (`unattended=true`) | 管理操作 (`OpManage`) |
|---|---|---|---|---|---|
| `off` | 关闭 / Off | 拒绝（对助手不可见） | 拒绝 | 拒绝 | 永远拒绝（仅限主人） |
| `read` | 只读 / Read | 允许 | 拒绝 | 仅读允许，写拒绝 | 永远拒绝（仅限主人） |
| `confirm` | 可代发（逐次确认） / Confirm | 允许 | 逐次确认 (`Confirm`) | 仅读允许，写拒绝 | 永远拒绝（仅限主人） |
| `follow` | 持续跟进 / Follow | 允许 | 持有精确匹配 `session_key` 且未过期、未耗尽轮次的 `FollowLease` 的 `OpSend`/`OpInterrupt` 直接放行（`Allow`）；其余会话、新开会话（`OpStart`）或租约到期/耗尽后自动降级回 `Confirm` | 仅当持有精确匹配 `session_key`、未过期、未耗尽轮次且 `allow_background=true` 的 `FollowLease` 时允许；否则拒绝 (`Deny`) | 永远拒绝（仅限主人） |
| `auto` | 直接放行 / Auto | 允许 | 在线直接放行 (`Allow`)，受全套安全护栏约束；仓库挂载与授权仍每次强制手机确认 | 仅读允许，写拒绝 | 永远拒绝（仅限主人） |

### 安全红线：`OpManage` 永远仅限主人（Owner-only）

以下管理操作归类为 `OpManage`，**在任何等级（包括 `follow` 与 `auto`）以及持有任何 `FollowLease` 的情况下，对助手永远返回 `Deny`**：

- 账号配对、解绑移除、替换或刷新凭据（API Key / 登录态）
- 修改账号的 `assistant_control` 权限等级或切换「主助手」开关
- 签发、修改或撤销 `FollowLease`（`club.leases.grant` / `club.leases.revoke`）
- 修改 GitHub 仓库授权范围或任何凭据保险箱（Vault）访问

---

## 二、按会话持续跟进租约（`FollowLease`）

当用户希望主 Agent 在**某一个指定对话**中围绕同一件事连续跟进多轮，而不必每发一句话都在手机上确认，同时又不希望放开整个账号的所有会话时，可将账号设为 `follow`（持续跟进），并为目标会话创建一条 `FollowLease`（实现于 `internal/core/follow.go` 与 `internal/store/`）。

### 1. 数据模型与持久化字段

每条 `FollowLease` 持久化在存储层（SQLite `agent_follow_leases` 表及内存 `Memory` 实现）：

- `lease_id`：租约唯一标识（如 `fl_...`）
- `account_id`：所属账号 ID
- `session_key`：精确绑定的会话标识（如 `agent:<provider>:<account_id>:<external_id>`，只读会话不可签发租约）
- `created_at` / `expires_at`：创建时间与到期时间戳（毫秒），支持常用档位 **`2h`**（默认）、**`6h`**、**`24h`**（上限 24 小时）
- `max_turns`：允许连续跟进的最大轮次上限，支持常用档位 **`10`**（默认）、**`20`**、**`50`**（上限 50 轮）
- `used_turns`：已消耗轮次，每次通过租约放行写操作时在存储层**原子递增**（SQLite 单语句条件 `UPDATE ... WHERE used_turns < max_turns RETURNING` / 内存互斥锁），严禁并发竞态超额
- `allow_background`：是否允许后台跟进；即使开启，也**必须同时受同一 `max_turns` 与 `expires_at` 双重封顶**
- `revoked_at` / `revoke_reason`：撤销时间戳与撤销原因（`manual`、`replaced`、`control_confirm`、`control_read`、`control_off`、`account_removed` 等）
- `note`：主人填写的跟进备注（可选）
- `status`：由当前时间与计数实时推导为 `active`（生效中）、`expired`（已到期）、`exhausted`（轮次已用尽）或 `revoked`（已撤销）

### 2. 生命周期与降级一致性

1. **同会话单活跃租约**：为同一 `session_key` 新建租约时，存储层在同一事务内自动将该会话此前未撤销的旧租约标记为已撤销（`revoke_reason = "replaced"`）。
2. **轮次耗尽或 TTL 到期自动降级**：
   - 当 `used_turns >= max_turns` 或 `now >= expires_at` 时，租约立即失效；
   - 在线写操作（`unattended=false`）**自动降级回 `confirm`（逐次手机确认）**；
   - 后台跟进操作（`unattended=true`）**立即拒绝（`Deny`）**。
3. **控制等级降级自动撤销（包含降级为 `confirm`、`read`、`off`）**：
   - 当主人将账号的 `assistant_control` 降级为 **`off`**、**`read`** 或 **`confirm`** 时（见 `core.RevokesLeasesOnControlChange` 与 `Space.UpdateAccount`），系统自动批量撤销该账号下所有活跃 `FollowLease`（原因记为 `control_off` / `control_read` / `control_confirm`）。
   - **关于降级为 `confirm` 的对齐说明**：用户显式将账号切回「可代发（逐次确认）」，代表要求该账号所有对话立即回到每次手机确认模式；立即撤销所有活跃租约可杜绝残留租约继续免确认发送，且再次切回 `follow` 时不会意外恢复旧租约。
4. **账号移除自动撤销与清理**：
   - 移除账号但保留聊天记录（`delete_data=false`）时，自动撤销该账号下所有未撤销租约（`revoke_reason = "account_removed"`）；
   - 移除账号并删除聊天记录（`delete_data=true`）时，一并物理删除该账号的租约记录。
5. **手动撤销**：主人可在账号详情页随时手动撤销单条租约或一键撤销该账号的全部租约。

---

## 三、`auto`（Bypass）模式与安全护栏

当主人希望某个个人 Agent 账号允许主助手在在线对话中直接发消息、新开会话或打断，而不必每次手机确认时，可将其切为 `auto`（直接放行）。

### 1. 显式二次确认（防误触落盘）

在账号页或配对完成页点选「直接放行」时，**不会立即落盘**，而是先在页面上弹出「确认开启直接放行」二次确认卡片，详细提示风险与四项生效护栏；只有主人在确认卡片上再次点击「确认开启直接放行」并确认弹窗后，才真正写入数据库。

### 2. 四道运行时安全护栏

即便在 `auto`（或持有 `FollowLease` 的 `follow`）模式下，每次写操作仍必须通过以下护栏（实现于 `internal/extension/tools.go` 与 `internal/core/follow.go`）：

1. **额度与冷却预检（Quota Pre-check）**：
   - 发送前检查账号是否存在已知额度耗尽或上游 429 限流冷却（`quotaExhaustedActive`）。若已耗尽，直接拦截并返回预计恢复时间或同步指引，绝不盲发。
2. **未确认发送防护（`LocalUnconfirmed` Guard）**：
   - 发送前通过 `Space.HasUnconfirmedSend` 检查目标会话是否存在状态为 `unconfirmed` 且尚未被远端消息回显的本地发送记录。
   - 若存在未确认消息，立即拦截后续自动发送并提示先调用 `club_agent_read` 或手动同步核对，防止在网络超时或状态不明时重复连发。
3. **单会话突发频率限制（Per-session Burst Rate Limit）**：
   - 按单会话（或新开会话按账号）维护滑动时间窗计数器（`BurstWindowMs = 60s`，`BurstMaxWrites = 5`）。
   - 短时间内高频突发写入达到上限时自动触发 `RATE_LIMITED` 拦截（幂等重试相同 `RequestID` 不重复扣减），防止 Agent 死循环高频刷屏。
4. **完整审计日志（Structured Audit Log）**：
   - 每次权限判定、租约签发/消耗/撤销、`auto` 放行以及护栏拦截均通过 `Space.RecordAudit` 记录结构化审计事件（含时间戳、账号、会话、工具、操作、控制等级、判定结果、租约轮次、护栏原因）。
   - **隐私红线**：审计日志与工具输出（包括 `club_agents_list`）严禁包含 API Key、Token、Cookie 等任何凭据字段。

---

## 四、与 Persona Hub 门禁的分工

1. **Manifest `confirm_always` 收窄与底线**：
   - `persona-extension.json` 中 `club` 连接器的 `confirm_always` 设定为：
     ```json
     "confirm_always": [
       "club_agent_repo_authorize",
       "club_agent_repo_bind"
     ]
     ```
   - 这意味着：**GitHub 仓库授权（`club_agent_repo_authorize`）与频道仓库挂载（`club_agent_repo_bind`）在 Persona Hub 层永远强制手机确认**，并且在扩展内部 `repos_tools.go` 中也显式锁定为 `Confirm`（绝不消耗 `FollowLease`、绝不标记 `Bypass=true`、后台任务永远拒绝）。
2. **常规写操作的双层协同**：
   - 对于 `club_agent_send`、`club_agent_start`、`club_agent_interrupt`：
     - **第一层（Persona Hub 网关）**：根据主人的 Hub 网关规则与会话上下文（在线交互 vs 后台任务）执行门禁调度；
     - **第二层（Ensemble 扩展内核与护栏）**：在 `actions.describe` 与 `actions.do` 双阶段执行 5 级权限矩阵、`FollowLease` 精确 `session_key` 校验与原子扣减、额度预检、`LocalUnconfirmed` 拦截、单会话突发限流以及二次重验（防止用户在确认卡片弹出期间降级了账号权限）。
