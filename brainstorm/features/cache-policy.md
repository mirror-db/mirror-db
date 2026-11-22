# 缓存策略下发

## 目标

扩展现有 mirrors 接口的 policy，使 relay 能判断 mirror 路径的探测能力、prefetch 行为及缓存相关配置，并以可覆盖的规则组合生效。

## 非目标

- 不改变 policy 已承担的 relay transport 选择目标。
- 不在本功能中执行具体的元数据解析或 prefetch 调度。

## 交互

- mirrors 接口下发路径是否允许 HEAD 等探测信息；不允许 HEAD 时由 relay 使用 GET 探测。
- policy 可标记特定 mirror 路径需要 prefetch 并自动保持更新。
- 多条规则按顺序求值：匹配规则将显式字段合并进当前配置，后面的匹配覆盖前面的值；匹配且带 `exit` 时，在合并后立即停止。
- 未出现的字段继承已有值，显式 `false`、空值或新值表示覆盖。

## 实现

- 复用现有 mirror、tag、path 匹配能力与现有 transport policy。
- 最终语义为按字段合并、last match win；`exit` 是求值控制字段。
- 结构化字段是否只做顶层浅合并、空值的具体表示及默认配置结构待定。
- 现有 relay 若仍按 first-match 解释同一 manifest，会产生不同结果；协议版本或兼容方式待定。

## 验收

1. 后匹配字段覆盖先匹配字段，未指定字段保持继承。
2. `exit` 仅在规则匹配时生效，并阻止后续规则参与合并。
3. transport、探测和 prefetch 配置可在同一求值流程中组合。
4. relay 能根据下发结果选择 HEAD 或 GET，并识别路径的 prefetch 意图。
5. 新旧 relay 的 policy 语义兼容方式在实现前明确。

## 状态

草案
