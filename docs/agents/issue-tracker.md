# Issue tracker: GitHub

本仓库的 issue 与规格文档都在 GitHub Issues 里（`knight000/go_qqbot`）。所有操作走 `gh` CLI。

## 约定

- **创建**：`gh issue create --title "..." --body-file <文件>`。多行正文写进文件再传，不要塞进 `--body`。
- **读取**：`gh issue view <n> --comments`。
- **列表**：`gh issue list --state open --json number,title,body,labels`，按 `--label` / `--state` 过滤。
- **评论**：`gh issue comment <n> --body "..."`。
- **打/去标签**：`gh issue edit <n> --add-label "..."` / `--remove-label "..."`。
- **关闭**：`gh issue close <n> --comment "..."`。

仓库由 `git remote -v` 推断，在 clone 内运行 `gh` 会自动识别。

## 把 PR 当作请求面

**PRs as a request surface: no.**（若将来把外部 PR 当特性请求对待，改成 `yes`，`/triage` 会读这个开关。）

## 技能说"publish to the issue tracker"时

创建一条 GitHub issue。

## 技能说"fetch the relevant ticket"时

`gh issue view <number> --comments`。

## 阻塞边

GitHub 原生 issue 依赖：

- 加边：`gh api --method POST repos/<owner>/<repo>/issues/<child>/dependencies/blocked_by -F issue_id=<blocker 的数据库 id>`。
  注意 `issue_id` 要的是**数据库 id**（`gh api repos/<o>/<r>/issues/<n> --jq .id`），不是 `#number`，也不是 `node_id`。
- 依赖不可用时退化为票体顶部一行 `Blocked by: #<n>, #<n>`。
- 一张票解除阻塞 = 它的所有 blocker 都已关闭。

## 本项目额外约定

- 提交完成后先 push 再关票（用户既定习惯）。
- 票是**tracer bullet**：纵向切穿所有层，每张能独立验证。
