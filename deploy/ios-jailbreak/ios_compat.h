/*
 * 编译 iPhone 版 PostgreSQL 时用 -include 加到每个源文件最前面（见 build.sh）。
 * iOS SDK 把 system() 标成「iOS 上不可用」，编译直接报错。PostgreSQL 只在 archive_command /
 * restore_command 和 psql 的 \! 里用它，我们都不用；这里用 posix_spawn 补一个，越狱手机上能跑。
 * rootless 越狱没有 /bin/sh，先找 /var/jb/bin/sh。
 */
#ifndef XIAOJU_IOS_COMPAT_H
#define XIAOJU_IOS_COMPAT_H

#include <stdlib.h>
#include <spawn.h>
#include <sys/wait.h>
#include <unistd.h>
#include <errno.h>

extern char **environ;

static inline int
xiaoju_ios_system(const char *command)
{
	const char *sh = access("/var/jb/bin/sh", X_OK) == 0 ? "/var/jb/bin/sh" : "/bin/sh";
	char	   *argv[4];
	pid_t		pid;
	int			status;

	if (command == NULL)
		return access(sh, X_OK) == 0;
	argv[0] = (char *) "sh";
	argv[1] = (char *) "-c";
	argv[2] = (char *) command;
	argv[3] = NULL;
	if ((errno = posix_spawn(&pid, sh, NULL, NULL, argv, environ)) != 0)
		return -1;
	while (waitpid(pid, &status, 0) < 0)
	{
		if (errno != EINTR)
			return -1;
	}
	return status;
}

#define system(cmd) xiaoju_ios_system(cmd)

#endif
