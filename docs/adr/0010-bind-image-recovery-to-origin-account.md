---
status: accepted
---

# Bind image conversation recovery to its originating Upstream Account

The user authorized repair of multi-account timeout recovery while retaining
Vue, API and data compatibility. A known image conversation is account-owned;
selecting another text-pool account can return 404 and cannot recover its assets.

The native image executor returns the stable Account Repository management ID
with its conversation as internal execution metadata. The existing Image Task
owner retains the optional `upstream_account_id` reference in its current JSON
file, reloads it and supplies it only through a private execution context on
resume. It stores no token, image input or resubmittable request. Public request
fields cannot supply/override this binding and the Vue task projection remains
unchanged. Older task files remain readable; Python readers may ignore the
optional metadata without changing their established format.

The Account Pool exclusively owns admission and current credentials/egress.
Read-only recovery reserves the same per-account image slot and image-egress
capacity, refreshes credentials only for that stable account when required, and
never selects another account. Existing quota exhaustion or image execution
cooldown does not block reading already-submitted work. Disabled, deleted or
unusable credentials fail with the existing controlled unavailable semantics.

For legacy tasks without the reference, the execution owner may consult a
bounded conversation-indexed set of existing Call Records. Only records for the
same User Key and an unambiguous conversation/account association are evidence.
Email-based legacy evidence must resolve to exactly one current management ID.
Missing, conflicting or deleted-account evidence fails closed without probing
other accounts. Recovered identity is retained by the same task owner. New final
and streamed Call Records retain existing account/conversation audit fields.

## Consequences

- No new table, Redis role, global account/conversation registry or dual write.
- Generation and recovery keep distinct lifecycles; recovery only polls and
  downloads, never invokes generation, account-switch retries or quota charging.
- Restart does not regenerate queued/running work. A persisted timeout can be
  resumed explicitly with its originating-account reference after restart.
- Preserve existing public envelopes/status fields and task ownership. Missing
  proof is an explicit safe failure, never a guess from pool order or sole account.
- Verify fixture failure paths and actual timeout-to-image recovery separately;
  audit transport requests without logging credentials or signed addresses.
