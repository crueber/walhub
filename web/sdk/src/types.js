/**
 * JSDoc @typedef blocks for the wire shapes (12_web_ui.md §1.1, shapes from
 * 07_api.md §3 = MASTER_RUST_SPEC §9.5). Editor IntelliSense only — no
 * runtime, no type-check step. Payloads stay plain JSON objects.
 *
 * @typedef {{name: string, sha: string}} Ref
 *
 * @typedef {{sha: string, parents: string[], author: string, author_email: string,
 *   author_date: string, committer: string, commit_date: string, subject: string,
 *   body: string, trailers: {key: string, value: string}[]}} Commit
 *
 * @typedef {{name: string, type: "blob"|"tree"|"commit", mode: string, size: number|null, sha: string}} TreeEntry
 *
 * @typedef {{ref: string, sha: string, path: string, kind: "branch"|"tag"|"commit"}} Resolve
 *
 * @typedef {{ref: string, sha: string, path: string,
 *   entries: TreeEntry[], commit?: Commit, readme?: {name: string, contents: string}}} Tree
 *
 * @typedef {{ref: string, sha: string, path: string, name: string, size: number,
 *   contents?: string, binary?: boolean, too_large?: boolean}} Blob
 *
 * @typedef {{ref: string, sha: string, commits: Commit[], more: boolean}} CommitPage
 *
 * @typedef {{commit: Commit, stats: {path: string, additions: number, deletions: number}[], patch: string}} CommitDetail
 *
 * @typedef {{refs: Ref[], more: boolean}} RefPage
 *
 * @typedef {{head: {name: string, sha: string}|null}} Refs
 *
 * @typedef {{owner: string, name: string, full_name: string, head: {name: string, sha: string}|null,
 *   branches: Ref[], tags: Ref[], clone_url: string, html_url: string, api_url: string}} RepoInfo
 *
 * @typedef {{status: "ok"|"degraded"|"error", issues: string[], deep: boolean,
 *   suggestions: {op: string, params?: Object, reason: string, auto?: boolean}[]}} WalHealth
 *
 * @typedef {{repo: string, clone_url: string, hostname: string, health: WalHealth,
 *   manifest: Object, local: Object, packs: Object,
 *   bundles: Object[], bundle_plan: Object, compactions: Object[], node: Object}} Overview
 *
 * @typedef {{id: string, kind: string, state: string, repo: string, at: string,
 *   detail?: Object, log?: string[]}} TaskRecord
 *
 * @typedef {{name: string, title: string, description: string,
 *   params: {name: string, kind: string, default?: any, choices?: any[]}[]}} OpSpec
 *
 * @typedef {{available: OpSpec[], recent: TaskRecord[], bundle_strategies: string[]}} OpsList
 *
 * @typedef {{hostname: string, running: TaskRecord[], recent: TaskRecord[]}} TasksList
 *
 * @typedef {{version: number, base: string, browser_base: string, sdk: string,
 *   auth: {bearer: boolean, setup: string, browser: string, authenticate: string},
 *   endpoints: string[]}} Discovery
 *
 * @typedef {{principal: string, write: boolean, anonymous: boolean}} Me
 *
 * @typedef {{revision: number, author: string, updated_at: string, message: string, toml: string}} Settings
 *
 * @typedef {{min_seq: number, entries: {seq: number, revision: number, author: string,
 *   message: string, at: string, toml: string}[]}} SettingsHistory
 *
 * @typedef {{version: number, principal: string, display_name: string, bio: string,
 *   created_at: string, updated_at: string}} Profile
 *
 * @typedef {{version: number, org: string, display_name: string, description: string,
 *   created_at: string, updated_at: string}} Org
 *
 * @typedef {{principal: string, role: "owner"|"member", joined_at: string}} Member
 *
 * @typedef {{version: number, members: Member[], updated_at: string}} Members
 *
 * @typedef {{version: number, org: string, slug: string, name: string, description: string,
 *   members: string[], created_at: string, updated_at: string}} Team
 *
 * @typedef {{subject: string, role: "read"|"triage"|"write"|"maintain"|"admin"}} AccessBinding
 *
 * @typedef {{version: number, visibility: "public"|"private",
 *   role_bindings: AccessBinding[]}} AccessDoc
 *
 * @typedef {{id: string, kind: "org"|"repo", org: string, repo: string, role: string,
 *   subject: string, invited_by: string, state: string, created_at: string,
 *   expires_at: string}} Invitation
 */

export {};
