# 损坏样本表对 MP4 容器定位结果的影响分析

仓库：`abema/go-mp4`（commit e4068d3）。本文分析当样本表（`stsc` 样本→块映射、`stco/co64` 块偏移、`stsz` 样本大小、`stts` 解码时间、`ctts` 合成时间偏移）互相矛盾时，库的三层接口分别在何处接受、拒绝或报告位置信息，并用可构造的最小容器实验区分"输入损坏被检出"与"接口职责缺口"。

## 1. 样本表如何参与定位

`Probe`（[probe.go](probe.go)）把四张表拼成 `Track` 的两条链：

- `stco`/`co64` → `Chunks[i].DataOffset`（probe.go:311-324）
- `stsc` → `Chunks[i].SamplesPerChunk`（probe.go:344-351）
- `stts` → `Samples` 切片（长度完全由 `stts` 决定）与 `TimeDelta`（probe.go:332-340）
- `stsz` → `Samples[i].Size`（probe.go:367-378）；`ctts` → `CompositionTimeOffset`（probe.go:353-364）

样本 *i* 的文件位置由调用方按下式推算（库内唯一的消费者是 `FindIDRFrames`，probe.go:603-633）：

```
sampleOffset(i) = Chunks[c].DataOffset + Σ Size(chunk c 内 i 之前的样本)
```

四张表任何一对互相矛盾，这个推算都会给出错误位置。问题在于：错误在哪一层被发现、以什么形式报告。

## 2. 三层接口的接受 / 拒绝 / 报告点

### 2.1 容器遍历层（read.go、box_info.go）——只做结构检查

`ReadBoxStructure` / `readBoxStructure` 按盒头 `(size, type)` 深度优先遍历，对样本表内容一无所知。拒绝点全部是盒结构性的：

- 子盒声明大小超过父盒剩余：`too large box size: type=..., size=..., actualBufSize=...`（read.go:198）
- 盒范围超过文件大小：`box size exceeds file size: ...`（`checkPayloadSize`，read.go:18；注意它先把读指针 seek 回盒头，且 seek 失败本身也会上抛）
- 非根级截断：`unexpected EOF`（read.go:221）；根级 EOF 是正常结束信号（read.go:190-191）

这一层对任何表间矛盾都**接受**，因为矛盾不表现为盒结构错误。

### 2.2 解析层（marshaller.go）——只校验盒内自洽

`Unmarshal` 按盒声明的 payload 大小 `u.size` 读取。动态数组（`stts.Entries`、`stsc.Entries`、`stsz.EntrySize`、`stco.ChunkOffset`）的长度由盒内 `EntryCount`/`SampleCount` 字段驱动；当计数超过 payload 实际能容纳的条目数时，读出界后报 `failed to read array completely: fieldName="..."`（marshaller.go:469）。注意读越界发生在**越过盒边界之后**——读取器会先吞掉后续盒的字节再报错，所以该错误只保证"计数与盒大小不符"，不保证文件其余部分完好。

### 2.3 探测层（probe.go `probeTrak`）——只检查"存在性"，不检查"一致性"

拒绝点（均为 `errors.New`，无类型、无包装）：

- 缺 `tkhd` / `mdhd`（probe.go:243、259）
- 缺 `stco` 且缺 `co64`：`stco/co64 box not found`（probe.go:326）
- 缺 `stts`：`stts box not found`（probe.go:330）
- 缺 `stsc`：`stsc box not found`（probe.go:342）

静默接受点（**接口职责缺口**所在）：

- **缺 `stsz` 不报错**：所有样本 `Size=0`，`Probe` 成功返回。
- **`stsc.FirstChunk` 越界或为 0**：循环 `for ci := entry.FirstChunk - 1; ci < end; ci++`（probe.go:349）中 `end` 被钳制为 `len(Chunks)`，`FirstChunk` 超过块数或为 0（`FirstChunk-1` 下溢为 2^32-1）时循环体不执行——不会 panic，但对应块 `SamplesPerChunk` 保持 0，无错误、无警告。`stsc` 条目乱序时同理（probe.go:346 的 `next.FirstChunk-1 < end` 判断只是防 panic，不防语义错误）。
- **`stsc` 隐含样本总数 ≠ `stts` 样本数**：从不交叉核对。`stsc` 多出时 `FindIDRFrames` 靠 `si < len(trackInfo.Samples)`（probe.go:610）静默截断；`stsc` 不足时部分块 `SamplesPerChunk=0`，其样本在定位时"消失"。
- **`stsz.EntrySize` 条目数 ≠ `stts` 样本数**：循环 `i < len(stsz.EntrySize) && i < len(track.Samples)`（probe.go:374）双向静默截断——少了尾部补 0，多了直接丢弃。`stsz.SampleCount` 字段**从未被读取核对**。
- **`stsz.SampleSize != 0`（恒定大小）模式**：`SampleCount` 被完全忽略，全部样本按 `stts` 数量赋同一大小（probe.go:368-372）。
- **`ctts` 条目多于 `stts` 样本**：`if si >= len { break }`（probe.go:358）静默截断。
- **`stco` 偏移从不与文件大小或 `mdat` 位置核对**：`checkPayloadSize` 只校验盒头，不校验表内容。坏偏移在 `Probe` 阶段被原样接受，直到 `FindIDRFrames` 的 `Seek`/`io.ReadFull`（probe.go:615-621）才以**通用 I/O 错误（如 `EOF`）**首次暴露——错误文本里没有任何"样本表损坏"的语义。

### 2.4 错误传播路径

所有层都不包装错误，原值上抛：

```
marshaller.Unmarshal            → "failed to read array completely: ..."
readBoxStructure(FromInternal)  → handler/Expand 错误原样返回（read.go:172-176）
ExtractBoxes(WithPayload)       → 原样返回（extract.go:23、37）
probeTrak / Probe               → 原样返回（probe.go:155-158）
FindIDRFrames                   → Seek/ReadFull 的 io 错误原样返回（probe.go:615-621）
```

后果：调用方拿到的 `error` 无法区分"输入损坏"（表计数越界、盒尺寸错误）与"环境故障"（I/O 错误），也无法区分错误出自哪张表（`fieldName="Entries"` 不说明是 `stts` 还是 `stsc`）。

## 3. 最小容器实验

构造方法：用库自身的 `Marshal` 生成最小 MP4——`ftyp` + `moov`（`mvhd` + `trak`：`tkhd`、`mdhd`、`minf/stbl`：`stsd(avc1/avcC)`、`stts`、`stsc`、`stsz`、`stco`）+ `mdat`。4 个样本 × 8 字节（4 字节 NAL 长度前缀 + NAL 头 + 3 字节，样本 0 为 IDR），2 块 × 2 样本，`stts` 4×1000。计数与 payload 矛盾的用例（库自身 `Marshal` 会拒绝生成，报 `the slice has too few elements`）用 `binary.BigEndian` 直接补丁字节。对每个变体调用 `Probe`、`FindIDRFrames`、`ReadBoxStructure`（全展开）记录结果。

### 3.1 输入损坏且被检出（拒绝）

| 用例 | 损坏内容 | Probe 结果 | 检出层 |
|---|---|---|---|
| F | `stsz.SampleCount=10`，payload 仅 4 条 | ERROR `failed to read array completely: fieldName="EntrySize"` | 解析层 |
| H | `stts.EntryCount=2`，payload 仅 1 条 | ERROR `failed to read array completely: fieldName="Entries"` | 解析层 |
| J | `stts` 盒 size=0xFFFFFF 超过 `stbl` 剩余 | ERROR `too large box size: type=stts, size=16777215, actualBufSize=112` | 遍历层 |
| K | `ftyp` size=0x7FFFFFFF 超过文件 | ERROR `box size exceeds file size: offset=0, size=2147483647, fileSize=561` | 遍历层 |
| I1 | 缺 `stsc` | ERROR `stsc box not found` | 探测层（存在性） |
| I3 | 缺 `stco` | ERROR `stco/co64 box not found` | 探测层（存在性） |
| I4 | 缺 `stts` | ERROR `stts box not found` | 探测层（存在性） |

F/H/J/K 中 `ReadBoxStructure` 全展开返回与 `Probe` 完全相同的错误，证实错误在遍历/解析层产生、经提取层原样上抛。

### 3.2 输入损坏但被静默接受（接口职责缺口）

| 用例 | 损坏内容 | Probe 结果 | FindIDRFrames 结果 |
|---|---|---|---|
| B | `stsc.FirstChunk=5` > 2 块 | OK；两块 `SamplesPerChunk=0` | `idxs=[]`，**无错误**（样本消失） |
| C | `stsc.FirstChunk=0`（下溢） | OK；两块 `SamplesPerChunk=0` | `idxs=[]`，无错误 |
| D | `stsc` 隐含 6 样本 vs `stts` 4 | OK；`SamplesPerChunk=3` | `idxs=[0]`，靠 `si<len(Samples)` 静默截断 |
| E | `stsz` 2 条 vs `stts` 4 样本 | OK；样本 2、3 `Size=0` | `idxs=[0]`；`Size=0` 样本被跳过，后续块内偏移推算已错位但不报错 |
| G | `stco` 偏移 0x7FFFFFFF 越出 EOF | OK；`DataOffset=2147483647` 原样接受 | `idxs=[]`，`err=EOF`——通用 I/O 错误，无表损坏语义 |
| I2 | 缺 `stsz` | OK；全部 `Size=0` | `idxs=[]`，无错误 |
| L | `ctts` 6 样本 vs `stts` 4 | OK；4 样本 `cto=40`，多余条目静默丢弃 | 正常 |
| M | `stsz` 恒定大小模式，`SampleCount=99` vs `stts` 4 | OK；4 样本 `Size=8`，`SampleCount` 被忽略 | 正常 |

这些用例的 `ReadBoxStructure` 全部返回 `nil` 错误：输入确实损坏（四张表互相矛盾），但三层接口都没有"跨表一致性"这一职责，也没有返回警告的渠道。定位结果分三种失败模式：

1. **样本静默消失**（B、C、I2）：`SamplesPerChunk=0` 或 `Size=0` 使定位循环跳过样本，调用方得到"更少的样本"而非错误；
2. **位置静默错位**（D、E、L、M 的提取方）：`Probe` 返回的 `Track` 内部自相矛盾，任何按 §1 公式推算位置的调用方会读到错误字节，且全程无错误信号；
3. **错误延迟且语义丢失**（G）：唯一会报错的矛盾也要等到样本提取层，以 `EOF` 这种与"样本表"无关的形式出现。

### 3.3 基线（用例 A，正常文件）

`Probe` 成功：4 样本（`size=8, delta=1000`）、2 块（`offset=529/545, samplesPerChunk=2`）；`FindIDRFrames → idxs=[0]`；`ReadBoxStructure` 无错误。与 §3.2 对比可见：同样的接口在正常输入下结果完整正确，缺口只在输入损坏时显现——库的定位是"低层 I/O 接口"（README 自述），跨表校验被有意留给调用方，但 `Probe` 的聚合返回值（`Track.Samples`/`Track.Chunks`）看起来已经是一致视图，调用方很容易误以为它已被校验。

## 4. 正常文件的既有探测结果如何保持

本次分析未修改任何库代码（实验用的临时测试文件已删除）。`go test ./...` 全部通过，既有探测行为由以下测试锁定：

- `TestProbe`（probe_test.go）：`testdata/sample.mp4` 的精确断言——2 条轨、视频轨 10 样本/9 块、`Chunks[0].DataOffset=48`、`SamplesPerChunk=2`、`stts` delta、`ctts` 偏移、`FindIDRFrames=[0]` 等；
- `TestProbeConstantSampleSize`：`sample_ac3.mp4` 走 `stsz.SampleSize!=0` 恒定大小路径（5 样本 × 384 字节），正是 §2.3 中忽略 `SampleCount` 的分支——该行为对正常 AC-3 文件是正确且被测试保护的；
- `TestProbeEncryptedVideo/Audio`、`TestProbeFragmented`、`sample_qt.mp4` 等变体：加密轨（`encv/enca`）、分片（`moof/traf` 走 `probeMoof`/`probeTraf`，其 `Duration`/`Size` 按 `trun` flag 回退到 `tfhd` 默认值，同样不与 `mdat` 实际大小核对）、QuickTime 兼容模式。

正常文件四表一致，§2.3 列出的所有防御性钳制（`end` 钳制、`si < len` 截断、`break`）都不会触发，因此这些静默路径的存在不改变任何正常文件的探测结果；它们只在输入损坏时把"应当报错"变成"静默给出错误定位"。

## 5. 结论

- **输入损坏被检出的边界**是：盒结构（遍历层）、盒内计数自洽（解析层）、关键盒存在性（探测层）。越过这三条边界的损坏都会得到明确错误，且错误原样穿过各层。
- **接口职责缺口**是：四张样本表之间、以及表与文件物理布局（`stco` vs 文件大小/`mdat`）之间的一致性，没有任何一层负责校验。`Probe` 聚合出的 `Track` 可能是自相矛盾的定位视图；`FindIDRFrames` 的防御性下界检查把矛盾吞成"样本消失"或"静默截断"；唯一会暴露的矛盾（坏 `stco`）也以无语义的 I/O 错误延迟到提取阶段。
- 调用方若需要可靠的定位结果，必须在使用 `Probe` 返回值前自行核对：`Σ SamplesPerChunk == len(Samples)`、`len(stsz.EntrySize) == len(Samples)`（或恒定大小模式）、每个 `stsc.FirstChunk` 落在 `[1, len(Chunks)]` 且单调、每个 `DataOffset + 块内样本总大小 <= 文件大小`。
