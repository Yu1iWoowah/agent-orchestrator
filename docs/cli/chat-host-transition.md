# Retire a surviving u3 Chat host

A daemon update can reattach to a persistent Chat host launched by
`woowah.u3`. That host retains its own delivery rules. An answered Claude
`_ao/session/can_hibernate` probe can leave a journal gap on u3. Shutdown
and idle hibernation can then refuse with `host_answered_without_record`.

Use the session's existing exit and resume operations after verifying that
the provider's work has ended. These operations stop the current agent and
preserve the AO session.

1. Identify the affected session and its current host. Verify the host's
   running build, PID, and start identity independently of the daemon.
2. If the probe already ran, confirm that it returned and that the original
   turn and its attempts reached a known terminal state. Investigate the
   original operation if the host only returns a generic refusal. Stop if
   an operation is unidentified, a provider turn is running, an attempt is
   still unconfirmed, or the host's ownership is inconclusive. Keep those
   attempts and their holds.
3. For a verified worker, use `ao session exit-agent <session-id>`. Check
   that the original host exited and released ownership. Then use
   `ao session resume-agent <session-id>` and verify that the session uses
   a new host running the installed build. For a reviewer, use its own
   authorized stop or archive workflow only after its work is known to
   have ended.
4. Retry idle hibernation or the original update or shutdown. Verify that
   it completes and that the original input was never sent again.

The same exit and resume sequence can replace a verified idle u3 worker
before it runs the probe, using the existing delivery journal.
