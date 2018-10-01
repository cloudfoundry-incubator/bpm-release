# v0.13.0

* volumes can be mounted inside `/var/vcap/sys/run`

# v0.12.3

* do not try and create cgroup directories if they already exist

# v0.12.2

* allow multiple volume options to be passed as a flag without quoting

# v0.12.1

* fix the environment variable flag from v0.12.0

# v0.12.0

* allow users to specify additional environment variables through flags on `bpm
  run`

# v0.11.0

* delete the pidfile when the job is shutting down

# v0.10.0

* allow users to specify additional volumes through flags on `bpm run`
* allow users to specify regular files in `additional_volumes`
* mount cgroup subsystems at canonical location

# v0.9.0

* decrease the time between SIGTERM and SIGKILL to 15 seconds from 20 seconds
* add the `mount_only` option for volumes

# v0.8.0

* add the `bpm run` command for executing processes as short-lived commands
* add support for unrestricted volumes in unsafe configuration

# v0.7.1

* sort mounts by ascending length of elements in destination path

# v0.7.0

* do not limit swap space on hosts which do not support it
* mounting reserved directories provides a more useful validation error
* remove the restriction on allowed capabilities

# v0.6.0

* change ownership of `/etc/profile.d/bpm.sh` to `vcap` group
* improved consistency of error messages
* add `/sbin` to the default system mounts
* add support for privileged containers

# v0.5.0

* mount cgroup subsystems when executing `bpm` command
* add `bpm version` command and global `bpm --version` flag
* include stopped processes in `bpm list` output
