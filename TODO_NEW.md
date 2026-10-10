# TODO_NEW — proposed features from Ansible / Terraform / Bolt / Salt / Puppet

Analysis date: 2026-10-10. Sources: Ansible docs (blocks, handlers, error
handling, delegation, strategies), Terraform docs (state, workspaces),
Puppet Bolt docs (plans, tasks, plugins), Salt docs (pillar).

## What goaf already covers

| Concept | Status in goaf |
|---|---|
| Ad-hoc commands (Bolt `run command`, `ansible`) | done |
| Playbook = Bolt plan / Ansible play | basic, done |
| `when`, `loop`, `notify/handlers`, `tags`, `serial`, `check`, `diff`, `vault`, `become` | done |
| NDJSON events (~ Ansible callbacks) + TUI | done |
| `register`, `failed_when`/`changed_when`, `ignore_errors` | done |

## 1. Quick wins from Ansible (small effort, big effect)

- `block` / `rescue` / `always` — try/catch for tasks (rollback when a
  deploy fails). Fits the existing runner; rescue sees registered vars.
- `pre_tasks` / `post_tasks` — e.g. drain LB -> deploy -> back to LB.
  Today this must be faked with separate plays.
- `run_once` + `delegate_to: localhost` — migrate DB once, send mail/slack
  notification from the controller. Impossible to express today.
- `flush_handlers` — run handlers mid-play (restart service before a
  health-check task).
- `max_fail_percentage` / `any_errors_fatal` — abort deploy when >X% of
  hosts fail; today a play always runs to the end.
- `retries` / `delay` / `until` — wait for a service after restart without
  hand-written shell loops.
- `debug` and `set_fact` modules — `debug: var=osline` (today needs
  `command: echo`) and computing variables mid-play.
- `script` and `fetch` modules — Bolt has these: upload a local script and
  execute it (today only inline `command`), or download a file from the
  host (today SFTP goes one way only).

## 2. Organization (Ansible roles / Bolt modules)

- `roles/` + `include_tasks` — a 200-line playbook becomes unmaintainable;
  a role = folder with tasks/handlers/templates/files/vars. Reusable
  across projects.
- Dynamic inventory — a script returning hosts (from Proxmox/VMware/cloud)
  instead of static YAML. Ideal for the alpine-docker + VMware lab:
  inventory straight from the hypervisors.
- Collections/modules path — load external modules without recompiling
  (today every module is hardcoded in the registry).

## 3. Best of Terraform

- `plan` file — `goaf run site.yml --save-plan=v2.plan`, then
  `goaf apply v2.plan`: review changes before production, audit trail,
  the same plan runs in CI. The strongest Terraform idea for goaf.
- Workspaces (`dev`/`stage`/`prod`) — same playbook+inventory, different
  workspace = different variable set; today files must be copied.
- State tracking — goaf is stateless (checks every time). Optional state
  (what was last applied, when) speeds up large fleets and gives drift
  detection.
- `output` values — a play prints e.g. IPs/deployed version at the end
  (today the log must be scraped).

## 4. Best of Salt / Puppet

- Facts cache (PuppetDB-style) — `gather_facts` costs one SSH round per
  host every run; cache for 1h + `--flush-cache`.
- Scheduled runs / watch mode (Salt reactors, reduced) —
  `goaf watch site.yml --every 15m` for drift remediation without cron syntax.
- `--confirm` prompt (AWX/Tower survey, reduced) — ask "sure?" before
  Apply on >N hosts; cheap production safety.
- Notifications/callbacks — webhook (Slack/mail) at run end with the
  recap; the emitter architecture already supports this.
- Audit log — every run (who, when, what, diff) into local SQLite —
  compliance and "who broke prod".

## 5. Recommended next branch (top 5, in order)

1. `block`/`rescue`/`always` + `pre`/`post_tasks` — the missing error handling.
2. `run_once` + `delegate_to: localhost` + `debug`/`set_fact` — foundation
   for serious playbooks.
3. `script` + `fetch` modules — closes the file-transfer story.
4. `plan` file (save/apply) — the Terraform workflow.
5. Dynamic inventory from VMware/Proxmox — direct benefit for the lab.
