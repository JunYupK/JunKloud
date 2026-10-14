# Vendored skills: superpowers

obra/superpowers 스킬을 플러그인 대신 프로젝트 스킬로 벤더링했다. 클라우드 세션에서 플러그인이 로드되지 않아, 레포에 포함시켜 모든 세션에서 바로 쓰도록 함.

- 출처: obra/superpowers **v7.0.0** (commit `bb92a77741419a4ab5f06e711a283343f1ada0c3`, 2026-10-09)
- 라이선스: MIT — `LICENSE-superpowers`
- 원본 파일은 수정하지 않음 (업데이트 시 그대로 덮어쓰기 위함)

## 포함 스킬 (11)

| 스킬 | 용도 |
| --- | --- |
| `brainstorming` | 구현 전 아이디어를 질문으로 구체화해 설계 확정 |
| `writing-plans` | 확정된 설계를 단계별 구현 계획으로 작성 |
| `executing-plans` | 작성된 계획을 체크포인트 단위로 실행 |
| `subagent-driven-development` | 계획의 태스크를 서브에이전트에 위임하고 리뷰 |
| `test-driven-development` | RED → GREEN → REFACTOR 사이클 |
| `systematic-debugging` | 근본 원인 추적 중심의 디버깅 절차 |
| `verification-before-completion` | 완료 주장 전 실제 검증 강제 |
| `requesting-code-review` | 작업 후 코드 리뷰 요청 |
| `receiving-code-review` | 리뷰 피드백 검증 후 반영 |
| `using-git-worktrees` | 격리된 워크트리에서 작업 |
| `finishing-a-development-branch` | 브랜치 마무리 (머지/PR/정리) |

제외: `diagnosing-superpowers` — 플러그인 설치 진단용이라 벤더링 환경에선 불필요.

스킬이 많을수록 설명이 잘리고 엉뚱한 스킬이 불릴 수 있어, 이 프로젝트에서 쓰지 않는 것도 뺐다: `using-superpowers`(모든 응답 전 스킬 호출을 강제하는 메타 스킬), `writing-skills`, `dispatching-parallel-agents`.

## 플러그인과의 차이

- 스킬 이름에 접두어가 없다. 문서 안의 `superpowers:<name>` 참조는 `<name>` 스킬로 읽는다.
- 플러그인의 SessionStart 훅(`using-superpowers` 자동 주입)은 포함하지 않는다 (`using-superpowers` 자체도 제외).

## 알려진 끊긴 참조

원본을 수정하지 않는 정책이라, 제외한 스킬을 가리키는 참조 두 곳은 그대로 남아 있다. 해당 경로를 읽으려 하지 말고 무시하면 된다.

- `executing-plans/SKILL.md:50` → `../using-superpowers/references/`
- `test-driven-development/writing-good-tests.md:51` → `superpowers:writing-skills`

## v7.0.0에서 달라진 점 (v6.4.2 대비)

- `brainstorming`이 재작성됐고, 선택적 **visual companion**이 붙었다 (`brainstorming/scripts/server.cjs`, `start-server.sh`, `stop-server.sh`). 로컬 Node와 포트 바인딩이 필요하므로 **웹/클라우드 세션에서는 동작하지 않을 수 있다**. 쓰지 않아도 스킬 본문은 그대로 작동한다.
- 셸 스크립트 8개가 실행 권한을 갖고 있다 (`brainstorming/scripts/*.sh`, `executing-plans/scripts/task-{start,done}`, `subagent-driven-development/scripts/{sdd-workspace,task-brief,review-package}`, `systematic-debugging/find-polluter.sh`). Windows 체크아웃에서는 실행 비트가 보존되지 않을 수 있으니, 그럴 때는 `bash <script>` 형태로 호출한다.

## 업데이트

```bash
git clone --depth 1 https://github.com/obra/superpowers.git /tmp/sp
for d in /tmp/sp/skills/*/; do
  n=$(basename "$d")
  case "$n" in diagnosing-superpowers|using-superpowers|writing-skills|dispatching-parallel-agents) continue ;; esac
  rm -rf ".claude/skills/$n" && cp -r "$d" ".claude/skills/$n"
done
cp /tmp/sp/LICENSE .claude/skills/LICENSE-superpowers
```

업데이트 후 위 버전/commit 표기를 갱신하고, 끊긴 참조 목록을 다시 확인한다:

```bash
grep -rnE "using-superpowers|writing-skills|dispatching-parallel-agents|diagnosing-superpowers" .claude/skills --include='*.md'
```
