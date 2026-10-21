# Go API compatibility inventory

Status: current

Baseline: `d58db042411f512449dd90e3b7a73de79ab38c46` (v3.2.3).
This is the source-derived migration checklist. Python remains the production
owner. The independently invoked Go server implements **112 of 112** method/path
pairs with an explicit rehearsal data directory. See
[the stage-4 guide](go-remaining-operations.md),
[the public-execution guide](go-public-execution.md) and
[control-plane guide](go-control-plane.md) for
verified coverage and limits; registration does not establish every input variant
or complete Vue workflow compatibility.

Run `python3 scripts/check_go_api_inventory.py` to detect missing, extra or
renamed method/path/handler entries. Explicit route declarations: **111**;
method/path pairs: **112**. FastAPI's automatic OpenAPI/docs/redoc routes and
implicit OPTIONS behavior are framework surfaces, tracked separately below.

## Contract rules applying to every row

- Preserve method, path, aliases, parameter locations/defaults/bounds, accepted
  media types, unknown-field handling, required fields, and validation status.
  Request model names below refer to current Python definitions; path/query
  declarations stay authoritative for defaults and constraints.
- Preserve absent/null/empty arrays and objects, number semantics, key names,
  Chinese business projections, capability flags, pagination, ordering and
  response headers. A dynamic response requires a captured contract before its
  Go replacement can pass; it is not an unrestricted schema.
- `identity` is a local Bearer User Key; `admin` additionally requires role admin.
  `/v1/messages` also accepts its existing `x-api-key` fallback. Login/status use
  the existing Bearer credential, not a new password/session scheme. Public
  asset routes retain their existing unguessable-path and ownership checks.
- `/v1/*` errors use the OpenAI error envelope, except `/v1/messages` which uses
  the Anthropic envelope. Management errors use `detail`. Preserve 401/403/422,
  Retry-After when emitted, and errors before/after SSE headers are sent.
- Preserve SSE event ordering, incremental content, terminal events, `[DONE]`,
  disconnect cancellation, partial success, and stream/nonstream equivalence.
- Preserve file MIME, filename/disposition, ZIP bytes, download headers,
  unpredictable public asset URLs, traversal rejection and owner boundaries.
- Keep Application Database table/column formats, account CAS revisions,
  SQLite BEGIN IMMEDIATE and PostgreSQL transaction semantics. Keep Image Tasks
  and Image Assets outside that database as specified in ADR 0004.

## Route checklist

Rows marked native identify implemented routes. Source symbols and declared
request/response models identify the compatibility owner. Offline fixture and
integration evidence is linked separately; native registration is not live or
full-console acceptance.

### [accounts.py](../api/accounts.py)

| Method | Path | Source handler | Access | Request parameters/models | Response model | Go stage |
| --- | --- | --- | --- | --- | --- | --- |
| `GET` | `/api/auth/users` | `accounts.py:list_user_keys` | admin | — | `UserKeyListView` | native stage 2 |
| `POST` | `/api/auth/users` | `accounts.py:create_user_key` | admin | body: UserKeyCreateRequest | `UserKeyCreateResult` | native stage 2 |
| `POST` | `/api/auth/users/{key_id}` | `accounts.py:update_user_key` | admin | key_id: str, body: UserKeyUpdateRequest | `UserKeyUpdateResult` | native stage 2 |
| `DELETE` | `/api/auth/users/{key_id}` | `accounts.py:delete_user_key` | admin | key_id: str | `UserKeyDeleteResult` | native stage 2 |
| `GET` | `/api/accounts` | `accounts.py:get_accounts` | admin | page: int, page_size: int, keyword: str, status: str, group_id: str | `dynamic` | native stage 2 |
| `POST` | `/api/accounts/selection-preview` | `accounts.py:preview_account_selection` | admin | body: AccountSelectionPreviewRequest | `dynamic` | native stage 2 |
| `GET` | `/api/accounts/{account_id}` | `accounts.py:get_account_detail` | admin | account_id: str | `dynamic` | native stage 2 |
| `POST` | `/api/accounts/{account_id}/test` | `accounts.py:test_account` | admin | account_id: str, body: AccountTestRequest | `AccountTestResult` | native stage 4 |
| `GET` | `/api/accounts/{account_id}/access-token` | `accounts.py:get_account_access_token` | admin | account_id: str | `dynamic` | native stage 2 |
| `GET` | `/api/accounts/{account_id}/refresh-token` | `accounts.py:get_account_refresh_token` | admin | account_id: str | `dynamic` | native stage 2 |
| `GET` | `/api/account-groups` | `accounts.py:list_account_groups` | admin | — | `dynamic` | native stage 2 |
| `POST` | `/api/account-groups` | `accounts.py:save_account_group` | admin | body: AccountGroupRequest | `dynamic` | native stage 2 |
| `DELETE` | `/api/account-groups/{group_id}` | `accounts.py:delete_account_group` | admin | group_id: str | `dynamic` | native stage 2 |
| `POST` | `/api/accounts` | `accounts.py:create_accounts` | admin | body: AccountCreateRequest | `dynamic` | native stage 2 |
| `DELETE` | `/api/accounts` | `accounts.py:delete_accounts` | admin | body: AccountDeleteRequest | `dynamic` | native stage 2 |
| `POST` | `/api/accounts/import-cleanup` | `accounts.py:cleanup_imported_abnormal_accounts` | admin | body: AccountImportCleanupRequest | `dynamic` | native stage 2 |
| `POST` | `/api/accounts/refresh-access-token` | `accounts.py:refresh_access_tokens` | admin | body: AccountOperationRequest | `dynamic` | native stage 2 |
| `POST` | `/api/accounts/sync` | `accounts.py:sync_accounts_and_quota` | admin | body: AccountOperationRequest | `dynamic` | native stage 2 |
| `POST` | `/api/accounts/refresh` | `accounts.py:sync_accounts_and_quota` | admin | body: AccountOperationRequest | `dynamic` | native stage 2 |
| `GET` | `/api/accounts/operations/{progress_id}` | `accounts.py:get_canonical_account_operation_progress` | admin | progress_id: str | `dynamic` | native stage 2 |
| `GET` | `/api/accounts/refresh/progress/{progress_id}` | `accounts.py:get_legacy_account_refresh_progress` | admin | progress_id: str | `dynamic` | native stage 2 |
| `POST` | `/api/accounts/export` | `accounts.py:export_accounts` | admin | body: AccountExportRequest | `dynamic` | native stage 2 |
| `POST` | `/api/accounts/update` | `accounts.py:update_account` | admin | body: AccountUpdateRequest | `dynamic` | native stage 2 |
| `POST` | `/api/accounts/batch-update` | `accounts.py:batch_update_accounts` | admin | body: AccountBatchUpdateRequest | `dynamic` | native stage 2 |
| `POST` | `/api/accounts/group` | `accounts.py:bind_accounts_group` | admin | body: AccountGroupBindRequest | `dynamic` | native stage 2 |
| `POST` | `/api/accounts/oauth/start` | `accounts.py:start_oauth_login` | admin | body: OAuthLoginStartRequest | `dynamic` | native stage 4 |
| `POST` | `/api/accounts/oauth/finish` | `accounts.py:finish_oauth_login` | admin | body: OAuthLoginFinishRequest | `dynamic` | native stage 4 |
| `GET` | `/api/cpa/pools` | `accounts.py:list_cpa_pools` | admin | — | `dynamic` | native stage 4 |
| `POST` | `/api/cpa/pools` | `accounts.py:create_cpa_pool` | admin | body: CPAPoolCreateRequest | `dynamic` | native stage 4 |
| `POST` | `/api/cpa/pools/{pool_id}` | `accounts.py:update_cpa_pool` | admin | pool_id: str, body: CPAPoolUpdateRequest | `dynamic` | native stage 4 |
| `DELETE` | `/api/cpa/pools/{pool_id}` | `accounts.py:delete_cpa_pool` | admin | pool_id: str | `dynamic` | native stage 4 |
| `GET` | `/api/cpa/pools/{pool_id}/files` | `accounts.py:cpa_pool_files` | admin | pool_id: str | `dynamic` | native stage 4 |
| `POST` | `/api/cpa/pools/{pool_id}/import` | `accounts.py:cpa_pool_import` | admin | pool_id: str, body: CPAImportRequest | `dynamic` | native stage 4 |
| `GET` | `/api/cpa/pools/{pool_id}/import` | `accounts.py:cpa_pool_import_progress` | admin | pool_id: str | `dynamic` | native stage 4 |
| `GET` | `/api/sub2api/servers` | `accounts.py:list_sub2api_servers` | admin | — | `dynamic` | native stage 4 |
| `POST` | `/api/sub2api/servers` | `accounts.py:create_sub2api_server` | admin | body: Sub2APIServerCreateRequest | `dynamic` | native stage 4 |
| `POST` | `/api/sub2api/servers/{server_id}` | `accounts.py:update_sub2api_server` | admin | server_id: str, body: Sub2APIServerUpdateRequest | `dynamic` | native stage 4 |
| `DELETE` | `/api/sub2api/servers/{server_id}` | `accounts.py:delete_sub2api_server` | admin | server_id: str | `dynamic` | native stage 4 |
| `GET` | `/api/sub2api/servers/{server_id}/groups` | `accounts.py:sub2api_server_groups` | admin | server_id: str | `dynamic` | native stage 4 |
| `GET` | `/api/sub2api/servers/{server_id}/accounts` | `accounts.py:sub2api_server_accounts` | admin | server_id: str, group_id: str &#124; None | `dynamic` | native stage 4 |
| `POST` | `/api/sub2api/servers/{server_id}/import` | `accounts.py:sub2api_server_import` | admin | server_id: str, body: Sub2APIImportRequest | `dynamic` | native stage 4 |
| `GET` | `/api/sub2api/servers/{server_id}/import` | `accounts.py:sub2api_server_import_progress` | admin | server_id: str | `dynamic` | native stage 4 |

### [ai.py](../api/ai.py)

| Method | Path | Source handler | Access | Request parameters/models | Response model | Go stage |
| --- | --- | --- | --- | --- | --- | --- |
| `GET` | `/v1/models` | `ai.py:list_models` | identity | — | `dynamic` | native stage 3 |
| `POST` | `/v1/images/generations` | `ai.py:generate_images` | identity | body: ImageGenerationRequest | `dynamic` | native stage 3 |
| `POST` | `/v1/images/edits` | `ai.py:edit_images` | identity | — | `dynamic` | native stage 3 |
| `POST` | `/v1/chat/completions` | `ai.py:create_chat_completion` | identity | body: ChatCompletionRequest | `dynamic` | native stage 3 |
| `POST` | `/v1/responses` | `ai.py:create_response` | identity | body: ResponseCreateRequest | `dynamic` | native stage 3 |
| `POST` | `/v1/messages` | `ai.py:create_message` | identity | body: AnthropicMessageRequest, x_api_key: str &#124; None, anthropic_version: str &#124; None | `dynamic` | native stage 3 |
| `POST` | `/v1/search` | `ai.py:search` | identity | body: SearchRequest | `dynamic` | native stage 3 |
| `GET` | `/v1/editable-file-tasks` | `ai.py:list_editable_file_tasks` | identity | ids: str, limit: int | `dynamic` | native stage 4 |
| `POST` | `/v1/editable-file-tasks` | `ai.py:create_editable_file_task` | identity | body: EditableFileTaskRequest | `dynamic` | native stage 4 |
| `DELETE` | `/v1/editable-file-tasks/{task_id}` | `ai.py:delete_editable_file_task` | identity | task_id: str | `dynamic` | native stage 4 |
| `GET` | `/files/{file_path:path}` | `ai.py:download_editable_file` | public | file_path: str | `dynamic` | native stage 4 |
| `POST` | `/v1/ppt/generations` | `ai.py:create_ppt_task` | identity | body: EditableFileTaskRequest | `dynamic` | native stage 4 |
| `POST` | `/v1/psd/generations` | `ai.py:create_psd_task` | identity | body: EditableFileTaskRequest | `dynamic` | native stage 4 |

### [app.py](../api/app.py)

| Method | Path | Source handler | Access | Request parameters/models | Response model | Go stage |
| --- | --- | --- | --- | --- | --- | --- |
| `GET` | `/{full_path:path}` | `app.py:serve_web` | public | full_path: str | `dynamic` | native stage 4 |
| `HEAD` | `/{full_path:path}` | `app.py:serve_web` | public | full_path: str | `dynamic` | native stage 4 |

### [image_tasks.py](../api/image_tasks.py)

| Method | Path | Source handler | Access | Request parameters/models | Response model | Go stage |
| --- | --- | --- | --- | --- | --- | --- |
| `GET` | `/api/image-tasks` | `image_tasks.py:list_image_tasks` | identity | ids: str | `ImageTaskPage` | native stage 3 |
| `POST` | `/api/image-tasks/generations` | `image_tasks.py:create_generation_task` | identity | body: ImageGenerationTaskRequest | `ImageTaskRow` | native stage 3 |
| `POST` | `/api/image-tasks/edits` | `image_tasks.py:create_edit_task` | identity | — | `ImageTaskRow` | native stage 3 |
| `POST` | `/api/image-tasks/{task_id}/resume-poll` | `image_tasks.py:resume_image_poll` | identity | task_id: str, body: ResumePollRequest | `ImageTaskRow` | native stage 3 |

### [prompts.py](../api/prompts.py)

| Method | Path | Source handler | Access | Request parameters/models | Response model | Go stage |
| --- | --- | --- | --- | --- | --- | --- |
| `GET` | `/api/prompts` | `prompts.py:list_prompts` | identity | — | `PromptLibraryView` | native stage 4 |
| `POST` | `/api/admin/prompt-sources/refresh` | `prompts.py:admin_refresh_prompt_sources` | admin | — | `PromptLibraryView` | native stage 4 |
| `POST` | `/api/admin/prompt-sources/{source_id}/refresh` | `prompts.py:admin_refresh_prompt_source` | admin | source_id: str | `PromptLibraryView` | native stage 4 |
| `POST` | `/api/admin/prompt-sources/{source_id}` | `prompts.py:admin_update_prompt_source` | admin | source_id: str, body: PromptSourceRequest | `PromptLibraryView` | native stage 4 |

### [system.py](../api/system.py)

| Method | Path | Source handler | Access | Request parameters/models | Response model | Go stage |
| --- | --- | --- | --- | --- | --- | --- |
| `POST` | `/auth/login` | `system.py:login` | identity | — | `AuthView` | native stage 2 |
| `GET` | `/auth/status` | `system.py:auth_status` | identity | — | `AuthView` | native stage 2 |
| `GET` | `/version` | `system.py:get_version` | public | — | `dynamic` | native stage 2 |
| `GET` | `/api/system/update-status` | `system.py:get_update_status` | admin | force: bool | `UpdateStatusView` | native stage 4 |
| `GET` | `/api/system/update-task` | `system.py:get_update_task` | admin | — | `UpdateTaskView` | native stage 4 |
| `POST` | `/api/system/update` | `system.py:start_update` | admin | — | `UpdateTaskView` | native stage 4 |
| `GET` | `/api/settings` | `system.py:get_settings` | admin | — | `SettingsView` | native stage 2 |
| `GET` | `/api/third-party-apps` | `system.py:get_third_party_apps` | identity | — | `PublicThirdPartyAppsView` | native stage 2 |
| `PATCH` | `/api/settings` | `system.py:save_settings` | admin | body: SettingsPatch | `SettingsMutationResult` | native stage 2 |
| `POST` | `/api/settings/retention-cleanup/preview` | `system.py:preview_retention_cleanup` | admin | body: RetentionCleanupRequest | `dynamic` | native stage 4 |
| `POST` | `/api/settings/retention-cleanup/run` | `system.py:run_retention_cleanup` | admin | body: RetentionCleanupRequest | `dynamic` | native stage 4 |
| `POST` | `/api/settings/account-cleanup/preview` | `system.py:preview_account_cleanup` | admin | body: AccountCleanupRequest | `dynamic` | native stage 4 |
| `POST` | `/api/settings/account-cleanup/run` | `system.py:run_account_cleanup` | admin | body: AccountCleanupRequest | `dynamic` | native stage 4 |
| `GET` | `/api/model-catalog` | `system.py:model_catalog` | identity | — | `ModelCatalogView` | native stage 2 |
| `GET` | `/api/images` | `system.py:get_images` | admin | start_date: str, end_date: str, page: int, page_size: int, media_type: GalleryMediaFilter, tag: str, search: str | `GalleryPage` | native stage 4 |
| `POST` | `/api/images/retention-cleanup` | `system.py:cleanup_expired_images_endpoint` | admin | — | `GalleryCleanupResult` | native stage 4 |
| `GET` | `/images/{image_path:path}` | `system.py:get_image` | public | image_path: str | `dynamic` | native stage 3 |
| `GET` | `/image-thumbnails/{image_path:path}` | `system.py:get_image_thumbnail` | public | image_path: str | `dynamic` | native stage 4 |
| `POST` | `/api/images/delete` | `system.py:delete_images_endpoint` | admin | body: ImageDeleteRequest | `dynamic` | native stage 4 |
| `POST` | `/api/images/download` | `system.py:download_images_endpoint` | admin | body: ImageDownloadRequest | `dynamic` | native stage 4 |
| `GET` | `/api/images/download/{image_path:path}` | `system.py:download_single_image_endpoint` | admin | image_path: str | `dynamic` | native stage 4 |
| `POST` | `/api/images/genbox-push` | `system.py:push_image_to_genbox` | admin | body: GalleryGenBoxPushRequest | `GalleryGenBoxPushResult` | native stage 4 |
| `GET` | `/api/logs` | `system.py:get_logs` | admin | type: str, start_date: str, end_date: str, status: CallLogStatus, endpoint: str, model: str, account: str, conversation_id: str, search: str, limit: int, offset: int | `CallSummaryPage` | native stage 4 |
| `GET` | `/api/logs/{log_id}` | `system.py:get_log_detail` | admin | log_id: str | `CallDetail` | native stage 4 |
| `POST` | `/api/logs/delete` | `system.py:delete_logs` | admin | body: LogDeleteRequest | `dynamic` | native stage 4 |
| `GET` | `/api/monitor/realtime` | `system.py:get_realtime_monitor` | admin | — | `RealtimeMonitorView` | native stage 4 |
| `GET` | `/api/monitor/realtime/{call_id}` | `system.py:get_realtime_monitor_call` | admin | call_id: str | `MonitorRecordDetailView` | native stage 4 |
| `POST` | `/api/proxy/test` | `system.py:test_proxy_endpoint` | admin | body: ProxyTestRequest | `dynamic` | native stage 2 |
| `GET` | `/api/proxy/view` | `system.py:get_proxy_view` | admin | — | `ProxyView` | native stage 2 |
| `POST` | `/api/proxy/defaults` | `system.py:save_proxy_defaults` | admin | body: ProxyDefaultsRequest | `ProxyDefaultsMutation` | native stage 2 |
| `POST` | `/api/proxy/groups` | `system.py:save_proxy_group` | admin | body: ProxyGroupPatch | `ProxyGroupMutation` | native stage 2 |
| `POST` | `/api/proxy/nodes/import` | `system.py:import_proxy_nodes` | admin | body: ProxyNodeImportRequest | `ProxyNodeImportResult` | native stage 2 |
| `DELETE` | `/api/proxy/groups/{group_id}` | `system.py:delete_proxy_group` | admin | group_id: str | `ProxyGroupDeleteMutation` | native stage 2 |
| `POST` | `/api/proxy/groups/test` | `system.py:test_proxy_group_endpoint` | admin | body: ProxyGroupTestRequest | `ProxyGroupTestResponse` | native stage 2 |
| `GET` | `/api/proxy/runtime` | `system.py:get_proxy_runtime_endpoint` | admin | — | `dynamic` | native stage 2 |
| `POST` | `/api/proxy/clearance/test` | `system.py:test_proxy_clearance_endpoint` | admin | body: ClearanceTestRequest | `dynamic` | native stage 4 |
| `GET` | `/api/dashboard` | `system.py:get_dashboard` | admin | — | `DashboardResponseView` | native stage 4 |
| `POST` | `/api/backup/test` | `system.py:test_backup_connection` | admin | — | `dynamic` | native stage 4 |
| `POST` | `/api/image-storage/test` | `system.py:test_image_storage_endpoint` | admin | — | `dynamic` | native stage 4 |
| `POST` | `/api/image-storage/sync` | `system.py:sync_image_storage_endpoint` | admin | — | `dynamic` | native stage 4 |
| `GET` | `/api/backups` | `system.py:get_backups` | admin | — | `dynamic` | native stage 4 |
| `POST` | `/api/backups/run` | `system.py:run_backup_endpoint` | admin | — | `dynamic` | native stage 4 |
| `POST` | `/api/backups/delete` | `system.py:delete_backup_endpoint` | admin | body: BackupDeleteRequest | `dynamic` | native stage 4 |
| `POST` | `/api/images/tags` | `system.py:update_image_tags` | admin | body: ImageTagsRequest | `dynamic` | native stage 4 |
| `GET` | `/api/images/storage` | `system.py:get_image_storage` | admin | — | `dynamic` | native stage 4 |
| `POST` | `/api/images/storage/compress` | `system.py:compress_all_images` | admin | — | `GalleryCompressResult` | native stage 4 |
| `POST` | `/api/images/storage/cleanup-to-target` | `system.py:cleanup_to_target` | admin | target_free_mb: int, dry_run: bool | `GalleryCleanupTargetResult` | native stage 4 |

## Additional compatibility gates

| Surface | Required behavior | Stage |
| --- | --- | --- |
| Framework HTTP | `/openapi.json`, `/docs`, `/docs/oauth2-redirect`, `/redoc`, CORS preflight and HEAD semantics; preserve or explicitly agree any replacement | 4 |
| Models | `/v1/models` and Studio consume the configured Model Catalog, not the probe's upstream model listing | 3 |
| Accounts/credentials | AT/RT projections, refresh distinct from quota sync, account group/proxy inheritance, bulk events and progress aliases | 2/4 |
| Tasks | User Key ownership, client task idempotency, bounded queue, restart interruption, resume-poll capability, partial results | 3 |
| Images | Only trusted tool outputs become assets; input attachments never count as generated outputs; preserve image-text-result classification | 3 |
| Logs/metrics | Final Call Record append in stage 3; queries, sequence/generation cursor management, aggregates and process-memory monitoring in stage 4 | 3/4 |
| External services | CPA/Sub2API import jobs, OAuth, proxy tests, WebDAV, GenBox and R2 backup retain side-effect and secret handling | 4 |
| Managed updates | Native Go manifest/platform/checksum validation, staged install and rollback; Python bundles rejected; opt-in CLI restart | 4; production 5 |
| Data cutover | Copy-based SQLite/PostgreSQL fixtures; table identities, JSON/time values, file indexes/tombstones, retention and rollback | 2–5 |
| Vue | Dashboard, Accounts, Settings, Proxy, Logs, Monitor, Gallery, Studio and login; desktop/mobile, retained snapshots, focus and scrolling | 4 |

## Verification levels

1. Inventory coverage only: method/path/source handler equality. Completed
   inventory coverage does **not** establish request or response parity.
2. Per-route contract fixtures: success, validation, authorization, errors,
   empty values, headers and streaming/file cases where applicable.
3. Isolated integration: datastore copies, concurrency/restarts and Vue checks.
4. Deliberate live probes and deployment rehearsal, reported independently.

The stage-1 prototype and its limitations are documented in
[the prototype guide](go-upstream-prototype.md). The ordered migration and
acceptance gates live in [PRD 0001](requirements/0001-go-backend-migration.md).
