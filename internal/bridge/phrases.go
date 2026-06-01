package bridge

import (
	"fmt"
	"hash/fnv"
)

var OnlineGreetings = []string{
	"早安，%s上线啦",
	"早安，%s已经在岗啦",
	"%s上线啦，今天也一起慢慢把事情做完",
}

var HealthGreetings = []string{
	"早安，%s在线。",
	"%s在线，健康检查也跑起来了。",
	"%s还醒着，来报个平安。",
}

var HealthCheckPrompts = []string{
	"请用一句不超过20个字的中文，轻松地确认 lark-bridge 健康检查正常。",
	"请用一句不超过20个字的中文，告诉用户 lark-bridge 状态正常。",
	"请用一句不超过20个字的中文，温和地确认 lark-bridge 还活着。",
}

var TaskCreatedWait = []string{
	"Task 已创建。请在这个话题里继续发送任务。",
	"Task 话题准备好了，接下来在这里发任务就行。",
	"新的 task 话题已经开好，我会把这里当成独立上下文。",
}

var TaskCreatedStart = []string{
	"Task 已创建，开始处理。",
	"Task 话题开好了，我这就开始处理。",
	"收到，新的 task 已经开工。",
}

var TaskPreparing = []string{
	"已收到任务，正在准备 Codex。\n\nmodel: %s\neffort: %s\ntier: %s",
	"收到，我先把 Codex 准备好。\n\nmodel: %s\neffort: %s\ntier: %s",
	"任务到手，正在热身。\n\nmodel: %s\neffort: %s\ntier: %s",
}

var CodexStarted = []string{
	"Codex 已启动，正在准备 session。",
	"Codex 已经起来了，正在接上 session。",
	"Codex 在线，正在整理上下文。",
}

var SessionReady = []string{
	"Codex session 已就绪，正在运行任务。",
	"session 接好了，Codex 开始干活。",
	"上下文准备好了，Codex 正在处理。",
}

var PendingBlocked = []string{
	"Codex 正在等待确认。请先回复 /approve、/deny 或 /cancel。",
	"这里需要你拍板。请回复 /approve、/deny 或 /cancel。",
	"Codex 卡在确认点上啦，请先回复 /approve、/deny 或 /cancel。",
}

var PendingResume = []string{
	"已收到确认，Codex 正在继续处理。",
	"确认收到，我让 Codex 继续往下跑。",
	"好，确认结果已转给 Codex。",
}

var PendingIntro = []string{
	"Codex 需要你确认：",
	"这里需要你看一眼：",
	"Codex 想先征求你的确认：",
}

var SteerDeferred = []string{
	"已收到修正，会在当前 Codex turn 就绪后插入。",
	"修正先收下，等当前 turn 能接收时马上塞进去。",
	"补充信息已记下，Codex 一就绪就转给它。",
}

var SteerInserted = []string{
	"已插入修正。",
	"修正已转给 Codex。",
	"补充信息已经塞进当前 turn。",
}

var EmptyResult = []string{
	"Codex 没有返回文本。",
	"Codex 这次没有留下文字结果。",
	"这次 Codex 没有生成可展示的文本。",
}

var LiveCardIntro = []string{
	"Codex 正在生成回答，下面是当前内容。",
	"Codex 还在写，先给你看当前进度。",
	"还在处理中，这是现在已经生成的部分。",
}

func PickPhrase(seed string, variants []string) string {
	if len(variants) == 0 {
		return ""
	}
	if len(variants) == 1 {
		return variants[0]
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(seed))
	return variants[int(hash.Sum32())%len(variants)]
}

func PickPhrasef(seed string, variants []string, args ...any) string {
	return fmt.Sprintf(PickPhrase(seed, variants), args...)
}
