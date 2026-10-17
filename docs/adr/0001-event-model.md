# 事件模型采用「大类 × 场景 × 平台事件名」三轴

`host.Event` 原先只有一个 `Type` 字段，它同时充当平台事件名和插件订阅键。平台推出的 `GROUP_MESSAGE_CREATE`（群消息全量模式）既不是已知订阅键、也不在解码表里，于是被静默丢弃，机器人在开启「接收所有消息」的群里完全失聪。现在事件由 `Kind`（`message` / `notice` / `request` / `meta` / `unknown`）、`Scene`（`group` / `c2c` / `guild`）、`Name`（平台原名）三个字段描述；解码表覆盖全部群与单聊事件；未知事件归为 `Kind=unknown` 并只做观测。

## Considered Options

- **合成单枚举**（`GroupMessage` / `PrivateMessage` / …）：枚举项随平台演进按「场景 × 类别」爆炸；而且 `Scene` 早已被 `api.SendMessage`、`messages.scene` 列与四处场景判断当作独立轴使用，合并它反而要重写出站契约。
- **保留 `Type` 并新增 `Name`**：`Type` 到底指归一化大类还是平台原名，这个歧义会永久固化在契约里，后来者必须靠注释才能判断。

## Consequences

- `host.APIVersion` 从 `"1"` 升到 `"2"`，不匹配的插件在装配阶段直接拒绝加载。当前全仓只有三个内置插件，是破坏成本最低的时机。
- `Manifest.Intents` 从平台事件名改为 `<kind>` 或 `<scene>.<kind>`（如 `message`、`group.message`）。插件因此无法订阅它没声明的数据，权限清单语义保持成立。
- 全量模式开启后，群里每条消息都会作为 `message` 事件到达并落库。这是使用者显式开启的能力，不是 bug；插件需要自行判断是否响应（`content` 已去掉 @机器人 的前缀）。
- `store.Message.MsgType` 开始写入平台真实值（`0` 文本 / `3` 卡片 / `101` 并行 / `102` 聊天记录 / `103` 引用），为将来的富消息模型留出数据基础。
