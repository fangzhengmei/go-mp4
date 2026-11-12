# 分片 MP4 默认样本值与媒体数据偏移的优先级分析

本文分析本仓库（github.com/abema/go-mp4）中分片 MP4（fMP4）探测流程对三类"默认值来源"
和"媒体数据偏移"的处理边界，并用相互冲突的最小容器实测验证。

## 1. 规范层面的优先级链

ISO/IEC 14496-12（ISOBMFF）为分片轨道定义了三级默认机制，作用于 moof/traf 内的每个样本：

样本属性（duration / size / flags）的取值优先级，从高到低：

1. **trun 显式条目**：trun 的 flag `0x000100`（sample-duration-present）、
   `0x000200`（sample-size-present）、`0x000400`（sample-flags-present）置位时，
   每个样本在 trun entry 中携带自己的值，直接生效。
   特例：`0x000004`（first-sample-flags-present）只覆盖该 trun 第一个样本的 flags。
2. **tfhd fragment 级默认**：tfhd 的 flag `0x000008`（default-sample-duration-present）、
   `0x000010`（default-sample-size-present）、`0x000020`（default-sample-flags-present）
   置位时，tfhd 携带的默认值作用于本 traf 内所有未被 trun 显式覆盖的样本。
3. **trex movie 级默认**：moov/mvex/trex 按 track_ID 匹配，为整条轨道兜底。
   只有当 trun 和 tfhd 都未提供对应值时才使用。

即：**trun 显式 > tfhd 默认 > trex 默认**。注意 tfhd 与 trex 中字段"是否存在"由 flag
位表达，字段值为 0 与字段不存在是两回事。

媒体数据偏移的解析链（与样本属性正交）：

1. tfhd flag `0x020000`（default-base-is-moof）置位时，base-data-offset 为**本 moof
   的起始偏移**，此时即使 `0x000001`（base-data-offset-present）同时置位，
   tfhd.BaseDataOffset 也必须被忽略（该字段在新版规范中已废弃）。
2. 否则若 `0x000001` 置位，base-data-offset 取 tfhd.BaseDataOffset（绝对文件偏移）。
3. 否则按隐式规则：moof 内第一个 traf 的 base-data-offset 为 moof 起始偏移；
   后续 traf 为前一个 traf 媒体数据的末尾。
4. trun flag `0x000001`（data-offset-present）置位时，trun.DataOffset 是**相对于
   base-data-offset 的有符号偏移**，样本数据起点 = base-data-offset + trun.DataOffset；
   未置位时，数据紧接前一个 trun 的数据（首个 trun 则从 base-data-offset 开始）。

## 2. 当前探测流程的实现边界

`Probe`（probe.go:106）对每个 moof 调用 `probeMoof`（probe.go:503），后者对每个 traf
调用 `probeTraf`（probe.go:521）。probeTraf 只提取三种 box：

```go
{BoxTypeTfhd()},
{BoxTypeTfdt()},
{BoxTypeTrun()},
```

由此产生以下责任边界：

1. **trex 完全不可见**。Probe 的 ExtractBoxes 路径里没有 moov/mvex/trex，
   probeTraf 也拿不到 moov 侧的任何上下文。当样本属性需要回落到 trex 时，
   probeTraf 只能拿到 tfhd 字段的零值，且**无法区分"字段不存在"与"字段为 0"**
   （Tfhd 结构体的可选字段是值类型，未置位时为零值）。
2. **Duration/Size 只有两级解析**。probe.go:564-579：
   trun 有 `0x000100`/`0x000200` 时累加显式条目；否则用
   `tfhd.DefaultSampleDuration * SampleCount` / `tfhd.DefaultSampleSize * SampleCount`。
   tfhd 未携带默认值时结果为 0，不会回落到 trex，也不会报错。
3. **媒体数据偏移完全不解析**。trun.DataOffset、tfhd.BaseDataOffset、
   default-base-is-moof 标志都被忽略。Segment 只记录 `MoofOffset`（moof 自身的
   文件偏移，来自 BoxInfo）和 `BaseMediaDecodeTime`（tfdt）。调用方无法从
   ProbeInfo 定位任何样本数据。
4. **多 trun 只保留最后一个**。probeTraf 的循环里 `trun = bip.Payload.(*Trun)`
   （probe.go:543）会被后续 trun 覆盖，Segment 的 SampleCount/Duration/Size/CTO
   只反映最后一个 trun，前面 trun 的样本静默丢失。
5. **Segment.Duration/Size 是 uint32**，超长 fragment 的累加值存在溢出风险；
   `CompositionTimeOffset` 只在最后一个 trun 的条目上取最小值。

对普通（非分片）MP4，track 侧信息来自 stbl（stts/stsz/stsc/stco/ctts），全部是
显式表项，不存在默认链，probeTrak 的解析是完整可靠的。上述边界只影响 moof 侧。

## 3. 冲突容器的构造与实测

为验证上述边界，用本仓库的 Writer/Marshal 构造了 5 个最小 fMP4 文件。公共结构：
ftyp + moov(mvhd, trak(tkhd, mdia(mdhd, minf(stbl(stts, stsc, stsz, stco)))),
mvex(trex)) + moof(mfhd, traf(tfhd, tfdt, trun[, trun])) + mdat。trex 恒为
duration=1000、size=100。各容器的冲突设置与 `Probe` 实测输出：

| 容器 | trex | tfhd | trun | 规范正确结果 | Probe 实测 | 结论 |
|---|---|---|---|---|---|---|
| A_trun_wins | dur=1000 size=100 | dur=2000 size=200 | 2×(dur=3000 size=300) | Duration=6000 Size=600 | Duration=6000 Size=600 | trun 显式优先，Probe 正确 |
| B_tfhd_wins | dur=1000 size=100 | dur=2000 size=200 | 无条目字段（2 样本） | Duration=4000 Size=400 | Duration=4000 Size=400 | tfhd 覆盖 trex，Probe 正确 |
| C_trex_only | dur=1000 size=100 | 无默认 flag | 无条目字段（2 样本） | Duration=2000 Size=200 | **Duration=0 Size=0** | trex 不可见，静默归零 |
| D_offset_conflict | — | base-data-offset=999999 且 default-base-is-moof | data-offset=64（2 样本） | 数据起点 = moof 起点 + 64（999999 必须被忽略） | 无任何数据偏移输出 | 偏移链完全未实现 |
| E_two_truns | — | dur=2000 size=200 | trun1: 2×(3000,300)；trun2: 7×(111,11) | 9 个样本，Duration=6777 Size=677 | **SampleCount=7 Duration=777 Size=77** | 只有最后一个 trun 生效 |

（构造程序位于 /tmp/fragexp/main.go，通过 go.mod 的 `replace` 指向本仓库运行；
Segment 的 MoofOffset=404 为 moof 自身的文件偏移，与样本数据位置无关。）

关键观察：

- C 容器是完全合法的 fMP4（大量真实编码器只写 trex），但 Probe 返回的
  Duration/Size 为 0，**不报错、无任何迹象表明值缺失**，调用方会把 0 当成真实值。
- D 容器中 tfhd 同时携带 base-data-offset=999999 和 default-base-is-moof，
  规范要求忽略前者；Probe 对两者都不解析，因此既不会算对也不会算错——
  它根本不承诺数据位置。
- E 容器说明"一个 traf 一个 trun"是 Probe 的隐含假设，规范并不保证这一点。

## 4. 当前流程能够可靠保证的结果

可以保证：

- moof 枚举：每个 moof 的每个 traf 产生一个 Segment，TrackID（tfhd 必填）、
  MoofOffset 正确；tfdt 存在时 BaseMediaDecodeTime 正确。
- 当 trun 携带显式 per-sample duration/size（flag 0x100/0x200）且该 traf 只有
  一个 trun 时，Duration/Size/SampleCount/CompositionTimeOffset 准确。
- 当 trun 无显式条目但 tfhd 携带默认值（flag 0x8/0x10）且只有一个 trun 时，
  Duration/Size 准确（= tfhd 默认 × SampleCount）。
- 普通 MP4 的全部 track 级信息（stts/stsz/stsc/stco/ctts 均为显式表）。

不能保证：

- 默认值需要回落到 trex 时，Duration/Size 静默为 0（与"真实值为 0"不可区分）。
- 一个 traf 含多个 trun 时，仅最后一个 trun 被统计。
- 任何样本数据的文件偏移（trun.DataOffset、tfhd.BaseDataOffset、
  default-base-is-moof 均未解析）。
- sample_flags / first_sample_flags 维度的优先级（Probe 不输出 flags，
  使用方若自行扩展需注意完整四级链）。

## 5. 扩展策略与拒绝策略的兼容性评估

### 扩展策略（读取 trex、补全默认链）

做法：Probe 增加 moov/mvex/trex 提取，按 track_ID 建立 trex 索引；probeTraf
在 tfhd 缺省时回落到 trex；同时解析 trun.DataOffset 与 tfhd 的偏移 flag，
并累加所有 trun。

兼容性影响：

- 对普通 MP4（无 moof）：零影响，新代码路径不触发。
- 对 tfhd 已携带完整默认值的 fMP4（如本仓库 testdata 中的样例）：结果不变，
  因为 trex 只在 tfhd 缺位时介入，优先级链保证不覆盖已有正确结果。
- 对 C 类文件（仅 trex）：输出从 0 变为真实值。这是行为变化，但原输出是
  静默错误，属于修复而非破坏；需要在 changelog 中明示。
- 必要的前置改动：Tfhd 的可选字段需要 presence 信息（导出 flag，或改用指针/
  增加 Present 布尔字段），否则"tfhd 显式默认 0"会被 trex 错误覆盖——
  这是扩展时唯一可能引入新错误的点。
- Segment.Duration/Size 改 uint64、多 trun 累加，均为向后兼容的结构/语义扩展。

### 拒绝策略（默认值解析不完整时返回错误）

做法：tfhd 与 trun 均未提供 duration/size 时，Probe 返回错误而非 0。

兼容性影响：

- 会把目前"能跑但结果为 0"的 C 类合法文件变成探测失败。对把 Probe 用于
  批量扫描、只关心 TrackID/时间的调用方，这是可用性回退。
- 普通 MP4 不受影响，但 fMP4 生态中"只靠 trex"的文件并不罕见，拒绝策略的
  误伤面大于收益。

### 结论

责任边界应划在：**解析器忠实报告每一级是否存在及取值，默认链的合并由解析器
按规范优先级完成，无法解析时显式标记缺失**。推荐采用扩展策略而非拒绝策略：
补全 trex 回落与偏移链、为可选字段增加 presence 标记、累加多 trun；
若短期不做扩展，至少应在 Segment 的文档注释中注明"Duration/Size 为 0 可能表示
默认值定义在 trex 中，本库未解析"，避免调用方把静默零值当成真实值。
