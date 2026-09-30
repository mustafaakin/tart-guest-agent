# Guest agent for Tart VMs

A guest agent for Tart VMS is a lightweight background service that runs inside the virtual machine and enables enhanced communication between the host and guest and other useful features, such as automatic disk resizing.

Currently implemented features:

* Automatic disk resizing for macOS VMs with recovery partition removed (`--resize-disk`)
    * needs to be invoked as a launchd [global daemon](https://launchd.info/)
* Clipboard sharing for macOS VMs using our in-house SPICE vdagent implementation (`--run-vdagent`)
    * needs to be invoked as a launchd [global agent](https://launchd.info/)
* `tart exec` support (`--run-rpc`)
    * it's recommended to invoke it as a launchd [global agent](https://launchd.info/) because fewer privileges will be available to commands started via `tart exec`
    * however, you can also invoke it as a launchd [global daemon](https://launchd.info/) if running commands started via `tart exec` as `root` is desired
* `tart ip --resolver=agent` support (`--run-rpc`)
    * allows resolving VM's IP address without relying on DHCP leases and/or an ARP table

To run all features appropriate for a given context, use component groups:

* `--run-daemon`
    * implies `--resize-disk` 
    * example usage: [`tart-guest-daemon.plist`](https://github.com/cirruslabs/macos-image-templates/blob/main/data/tart-guest-daemon.plist)
* `--run-agent`
    * implies `--run-vdagent --run-rpc` 
    * example usage: [`tart-guest-agent.plist`](https://github.com/cirruslabs/macos-image-templates/blob/main/data/tart-guest-agent.plist)

## Wrapping RPC commands

An image administrator can configure a fixed command prefix with repeated
`--exec-wrapper` flags. The guest agent appends the requested executable and its
arguments without shell interpolation. The prefix applies to every Exec RPC,
including interactive, PTY, detached, and user-override commands; clients cannot
disable it. With no prefix, execution is unchanged.

For example, a managed image can add an environment variable to every command:

```sh
tart-guest-agent --run-agent \
  --exec-wrapper=/usr/bin/env \
  --exec-wrapper=-- \
  --exec-wrapper=MANAGED_IMAGE=example
```

The first argument must be an absolute path to a regular file that the guest
agent's effective user can execute. Invalid configuration stops startup. A wrapper
that cannot start never falls back to running the requested command directly.
Attached commands return the wrapper's
exit status; detached commands retain their existing process-start acknowledgment.
Use a wrapper that replaces itself with the command so signals and exit handling
retain their usual behavior.

The wrapper receives the command name unchanged and handles its executable
lookup. Include its end-of-options marker in the prefix when its interface
requires one.

The wrapper runs with the command's requested environment, working directory,
and user. A requested user must also be able to execute the wrapper. Keep its
executable, configuration, and launch settings under the image administrator's
control, and choose a wrapper whose behavior remains correct
under those overrides. Only commands started through the guest agent use this
prefix.

## Windows guests

On Windows 11 on ARM guests, the agent supports `tart exec`, clipboard sharing and
`tart ip --resolver=agent`. Disk resizing isn't supported; extend `C:` from inside
Windows instead, for example with `Resize-Partition`.

Build it with:

```sh
GOOS=windows GOARCH=arm64 go build -o tart-guest-agent.exe ./cmd
```

It needs these [virtio-win](https://github.com/virtio-win/virtio-win-pkg-scripts) drivers:

* `viosock` for `--run-rpc`. Its INF also installs the `VirtioSocketWSP` service, which
  registers the Winsock provider for `AF_VSOCK`. Check that it's running with
  `Get-Service VirtioSocketWSP`.
* `vioserial` for `--run-vdagent`, which opens `\\.\Global\com.redhat.spice.0`. Only one
  process can open that port, so don't also run spice-vdagent from the virtio-win guest tools.

Run the agent in the signed-in user's session rather than as a service: the clipboard
belongs to that session, and commands started via `tart exec` can then use the desktop.
In a VM that signs in automatically, a scheduled task that starts at logon works:

```powershell
$action = New-ScheduledTaskAction -Execute "$env:SystemRoot\System32\conhost.exe" `
  -Argument '--headless "C:\Program Files\Tart\tart-guest-agent.exe" --run-agent'
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) `
  -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1)
$principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Highest
Register-ScheduledTask -TaskName "Tart Guest Agent" -Action $action -Trigger $trigger `
  -Settings $settings -Principal $principal
```

* `conhost.exe --headless` starts the agent without a console window. Its log output then
  goes nowhere, so run the agent from a terminal with `--debug` to troubleshoot.
* `-ExecutionTimeLimit` removes Task Scheduler's default 72-hour limit.
* `-RunLevel Highest` runs commands elevated. Without it, they run with the user's
  standard token.

Commands behave as on Unix, with these differences:

* They run as the agent's user; requests to run them as another user fail.
* TTY sessions use a ConPTY pseudo console.
* Each command runs in its own job object. Signal requests and canceled commands terminate
  the whole job immediately, since Windows can't ask an arbitrary process to exit. A signaled
  command exits with 143 for SIGTERM and 137 for SIGKILL, as on Unix.
* Detached commands get their own console, outside the agent's job where Windows allows it.
* `--exec-wrapper` must name an `.exe`, since Windows runs batch files through `cmd.exe`,
  which would reinterpret the arguments. Windows has no `exec()`, so the wrapper remains the
  command's parent and shares its job.
