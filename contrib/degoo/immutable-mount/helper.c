// SPDX-License-Identifier: MIT
// Copyright (C) 2026 The rclone-degoo contributors

#define _GNU_SOURCE

#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mount.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/statfs.h>
#include <sys/sysmacros.h>
#include <sys/types.h>
#include <sys/uio.h>
#include <unistd.h>

#define IMMUTABLE_MOUNTPOINT "/mnt/degoo"
#define MAX_OPTIONS_LENGTH 4096
#define MAX_TOKEN_LENGTH 2048
#define MAX_READ_LIMIT (1024U * 1024U)
/* linux/fs.h imports mount flags that conflict with sys/mount.h. */
#define FS_IOC_GETFLAGS _IOR('f', 1, long)
#define FS_IMMUTABLE_FL 0x00000010L
/* Linux's FUSE superblock magic is not exported in linux/magic.h. */
#define RCLONE_FUSE_SUPER_MAGIC 0x65735546L

struct mount_options {
	bool read_only;
	bool default_permissions;
	bool have_max_read;
	unsigned int max_read;
};

static void set_error(char *error, size_t error_size, const char *message)
{
	if (error_size != 0)
		(void)snprintf(error, error_size, "%s", message);
}

static int parse_decimal(const char *value, unsigned int *number)
{
	unsigned long result = 0;
	const unsigned char *p = (const unsigned char *)value;

	if (*p == '\0')
		return -1;
	for (; *p != '\0'; ++p) {
		unsigned long digit;

		if (*p < '0' || *p > '9')
			return -1;
		digit = (unsigned long)(*p - '0');
		if (result > (ULONG_MAX - digit) / 10)
			return -1;
		result = result * 10 + digit;
		if (result > UINT_MAX)
			return -1;
	}
	if (result == 0)
		return -1;
	*number = (unsigned int)result;
	return 0;
}

static int process_option(const char *token, size_t token_length,
			  size_t equals_at, struct mount_options *options,
			  bool *have_fsname, bool *have_subtype,
			  char *error, size_t error_size)
{
	const char *value = NULL;
	bool has_value = equals_at != SIZE_MAX;
	size_t key_length = has_value ? equals_at : token_length;

	if (token_length == 0) {
		set_error(error, error_size, "empty FUSE mount option");
		return -1;
	}
	if (has_value)
		value = token + equals_at + 1;
	if (key_length == 0) {
		set_error(error, error_size, "empty FUSE mount option name");
		return -1;
	}

#define KEY_IS(name) (key_length == sizeof(name) - 1 && memcmp(token, name, sizeof(name) - 1) == 0)
	if (KEY_IS("allow_other")) {
		set_error(error, error_size, "allow_other is not permitted for this mount");
		return -1;
	}
	if (KEY_IS("allow_root")) {
		set_error(error, error_size, "allow_root is not permitted for this mount");
		return -1;
	}
	if (KEY_IS("fsname")) {
		if (!has_value || *have_fsname) {
			set_error(error, error_size, "invalid or repeated fsname option");
			return -1;
		}
		*have_fsname = true;
		return 0;
	}
	if (KEY_IS("subtype")) {
		if (!has_value || *have_subtype) {
			set_error(error, error_size, "invalid or repeated subtype option");
			return -1;
		}
		*have_subtype = true;
		return 0;
	}
	if (KEY_IS("ro")) {
		if (has_value || options->read_only) {
			set_error(error, error_size, "invalid or repeated ro option");
			return -1;
		}
		options->read_only = true;
		return 0;
	}
	if (KEY_IS("default_permissions")) {
		if (has_value || options->default_permissions) {
			set_error(error, error_size, "invalid or repeated default_permissions option");
			return -1;
		}
		options->default_permissions = true;
		return 0;
	}
	if (KEY_IS("max_read")) {
		unsigned int max_read;

		if (!has_value || options->have_max_read || parse_decimal(value, &max_read) != 0) {
			set_error(error, error_size, "invalid or repeated max_read option");
			return -1;
		}
		options->have_max_read = true;
		options->max_read = max_read > MAX_READ_LIMIT ? MAX_READ_LIMIT : max_read;
		return 0;
	}

	set_error(error, error_size, "unsupported FUSE mount option");
	return -1;
#undef KEY_IS
}

static int parse_options(const char *text, struct mount_options *options,
			 char *error, size_t error_size)
{
	char token[MAX_TOKEN_LENGTH];
	size_t token_length = 0;
	size_t equals_at = SIZE_MAX;
	bool have_fsname = false;
	bool have_subtype = false;
	bool after_comma = false;
	const char *p;

	memset(options, 0, sizeof(*options));
	if (text == NULL || strlen(text) > MAX_OPTIONS_LENGTH) {
		set_error(error, error_size, "invalid or oversized FUSE mount options");
		return -1;
	}
	for (p = text;; ++p) {
		unsigned char c = (unsigned char)*p;

		if (c == '\\') {
			++p;
			if (*p != '\\' && *p != ',') {
				set_error(error, error_size, "invalid escape in FUSE mount options");
				return -1;
			}
			c = (unsigned char)*p;
		} else if (c == ',' || c == '\0') {
			if (token_length != 0 && process_option(token, token_length, equals_at,
							 options, &have_fsname, &have_subtype,
							 error, error_size) != 0)
				return -1;
			if (token_length == 0 && (c == ',' || after_comma)) {
				set_error(error, error_size, "empty FUSE mount option");
				return -1;
			}
			token_length = 0;
			equals_at = SIZE_MAX;
			after_comma = c == ',';
			if (c == '\0')
				break;
			continue;
		}
		if (token_length >= sizeof(token) - 1) {
			set_error(error, error_size, "oversized FUSE mount option");
			return -1;
		}
		if (c == '=' && equals_at == SIZE_MAX)
			equals_at = token_length;
		token[token_length++] = (char)c;
		token[token_length] = '\0';
		after_comma = false;
	}
	return 0;
}

#ifndef UNIT_TEST

static int is_unmount_request(int argc, char **argv)
{
	int i;
	bool options_ended = false;

	for (i = 1; i < argc; ++i) {
		const char *arg = argv[i];
		const char *p;

		if (options_ended)
			continue;
		if (strcmp(arg, "--") == 0) {
			options_ended = true;
			continue;
		}
		if (strcmp(arg, "-o") == 0) {
			++i;
			continue;
		}
		if (arg[0] != '-' || arg[1] == '\0' || arg[1] == '-')
			continue;
		for (p = arg + 1; *p != '\0'; ++p) {
			if (*p == 'u' || *p == 'z')
				return 1;
		}
	}
	return 0;
}

static const char *find_target(int argc, char **argv)
{
	int i;

	if (argc > 1 && argv[1][0] == '/')
		return argv[1];
	for (i = 1; i + 1 < argc; ++i) {
		if (strcmp(argv[i], "--") == 0)
			return argv[i + 1];
	}
	return NULL;
}

static int decode_mount_request(int argc, char **argv,
				const char **target, const char **option_text)
{
	if (argc == 5 && strcmp(argv[1], "-o") == 0 &&
	    strcmp(argv[3], "--") == 0) {
		*option_text = argv[2];
		*target = argv[4];
		return 0;
	}
	if (argc == 4 && strcmp(argv[2], "-o") == 0) {
		*target = argv[1];
		*option_text = argv[3];
		return 0;
	}
	return -1;
}

static int delegate_to_system_helper(int argc, char **argv, bool mount_request)
{
	static const char *const candidates[] = {
		"/usr/bin/fusermount3",
		"/bin/fusermount3",
		"/usr/bin/fusermount",
		"/bin/fusermount",
	};
	char path_environment[] = "PATH=/usr/bin:/bin";
	char language_environment[] = "LANG=C";
	char communication_environment[64];
	char *environment[4] = {path_environment, language_environment, NULL, NULL};
	struct stat self_stat;
	uid_t uid = getuid();
	gid_t gid = getgid();
	size_t i;
	int exec_error = ENOENT;
	bool found_candidate = false;
	const char *communication_fd = mount_request ? getenv("_FUSE_COMMFD") : NULL;
	(void)argc;

	if (communication_fd != NULL) {
		int length = snprintf(communication_environment, sizeof(communication_environment),
				      "_FUSE_COMMFD=%s", communication_fd);
		if (length < 0 || (size_t)length >= sizeof(communication_environment)) {
			fputs("fusermount3: invalid _FUSE_COMMFD value\n", stderr);
			return 1;
		}
		environment[2] = communication_environment;
	}

	if (stat("/proc/self/exe", &self_stat) != 0) {
		perror("fusermount3: cannot identify this helper");
		return 1;
	}
	for (i = 0; i < sizeof(candidates) / sizeof(candidates[0]); ++i) {
		struct stat candidate_stat;
		const char *path = candidates[i];

		if (stat(path, &candidate_stat) != 0)
			continue;
		if (candidate_stat.st_dev == self_stat.st_dev &&
		    candidate_stat.st_ino == self_stat.st_ino)
			continue;
		if (!S_ISREG(candidate_stat.st_mode) || candidate_stat.st_uid != 0 ||
		    (candidate_stat.st_mode & (S_IWGRP | S_IWOTH)) != 0)
			continue;
		found_candidate = true;
		if (setresgid(gid, gid, gid) != 0 || setresuid(uid, uid, uid) != 0) {
			perror("fusermount3: cannot drop privileges before delegation");
			return 1;
		}
		execve(path, argv, environment);
		exec_error = errno;
	}
	if (found_candidate) {
		errno = exec_error;
		perror("fusermount3: cannot execute a system FUSE helper");
	} else {
		fputs("fusermount3: no safe system FUSE helper is installed\n", stderr);
	}
	return 1;
}

enum mountpoint_result {
	MOUNTPOINT_REJECT = -1,
	MOUNTPOINT_CUSTOM = 0,
	MOUNTPOINT_DELEGATE = 1,
};

static int open_immutable_mountpoint(uid_t uid, int *mountpoint_fd)
{
	struct stat parent_stat;
	struct stat target_stat;
	struct statfs filesystem_stat;
	int parent_fd = -1;
	int target_fd = -1;
	long flags;
	DIR *directory = NULL;
	struct dirent *entry;
	int result = MOUNTPOINT_DELEGATE;

	parent_fd = open("/mnt", O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
	if (parent_fd < 0 || fstat(parent_fd, &parent_stat) != 0 ||
	    !S_ISDIR(parent_stat.st_mode) || parent_stat.st_uid != 0 ||
	    (parent_stat.st_mode & (S_IWGRP | S_IWOTH)) != 0) {
		goto out;
	}
	target_fd = openat(parent_fd, "degoo", O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
	if (target_fd < 0 || fstat(target_fd, &target_stat) != 0 ||
	    !S_ISDIR(target_stat.st_mode))
		goto out;
	if (fstatfs(target_fd, &filesystem_stat) != 0 ||
	    (unsigned long)filesystem_stat.f_type == (unsigned long)RCLONE_FUSE_SUPER_MAGIC) {
		fputs("fusermount3: /mnt/degoo is already a FUSE mount or its filesystem could not be checked\n", stderr);
		result = MOUNTPOINT_REJECT;
		goto out;
	}
	if (ioctl(target_fd, FS_IOC_GETFLAGS, &flags) != 0 ||
	    (flags & FS_IMMUTABLE_FL) == 0)
		goto out;
	if (target_stat.st_uid != uid ||
	    (target_stat.st_mode & (S_IRUSR | S_IXUSR)) != (S_IRUSR | S_IXUSR) ||
	    (target_stat.st_mode & (S_IWGRP | S_IWOTH)) != 0) {
		fputs("fusermount3: immutable /mnt/degoo must be caller-owned with owner read/execute and no group/world write access\n", stderr);
		result = MOUNTPOINT_REJECT;
		goto out;
	}
	directory = fdopendir(dup(target_fd));
	if (directory == NULL) {
		fputs("fusermount3: cannot inspect /mnt/degoo contents\n", stderr);
		result = MOUNTPOINT_REJECT;
		goto out;
	}
	errno = 0;
	while ((entry = readdir(directory)) != NULL) {
		if (strcmp(entry->d_name, ".") != 0 && strcmp(entry->d_name, "..") != 0) {
			fputs("fusermount3: refusing to cover a non-empty /mnt/degoo directory\n", stderr);
			result = MOUNTPOINT_REJECT;
			goto out;
		}
	}
	if (errno != 0) {
		fputs("fusermount3: error while reading /mnt/degoo contents\n", stderr);
		result = MOUNTPOINT_REJECT;
		goto out;
	}
	*mountpoint_fd = target_fd;
	target_fd = -1;
	result = MOUNTPOINT_CUSTOM;

out:
	if (directory != NULL)
		(void)closedir(directory);
	if (target_fd >= 0)
		(void)close(target_fd);
	if (parent_fd >= 0)
		(void)close(parent_fd);
	return result;
}

static int open_fuse_device(void)
{
	struct stat st;
	int fd = open("/dev/fuse", O_RDWR | O_CLOEXEC | O_NOFOLLOW);

	if (fd < 0) {
		perror("fusermount3: cannot open /dev/fuse");
		return -1;
	}
	if (fstat(fd, &st) != 0 || !S_ISCHR(st.st_mode) ||
	    major(st.st_rdev) != 10 || minor(st.st_rdev) != 229) {
		fputs("fusermount3: /dev/fuse is not the expected FUSE character device\n", stderr);
		(void)close(fd);
		return -1;
	}
	return fd;
}

static int get_communication_socket(uid_t uid)
{
	const char *text = getenv("_FUSE_COMMFD");
	unsigned long value = 0;
	const unsigned char *p;
	struct ucred peer;
	socklen_t length = sizeof(peer);
	int socket_type;
	int socket_fd;
	socklen_t socket_type_length = sizeof(socket_type);

	if (text == NULL || *text == '\0') {
		fputs("fusermount3: _FUSE_COMMFD is required\n", stderr);
		return -1;
	}
	for (p = (const unsigned char *)text; *p != '\0'; ++p) {
		unsigned long digit;

		if (*p < '0' || *p > '9') {
			fputs("fusermount3: invalid _FUSE_COMMFD descriptor\n", stderr);
			return -1;
		}
		digit = (unsigned long)(*p - '0');
		if (value > (INT_MAX - digit) / 10) {
			fputs("fusermount3: invalid _FUSE_COMMFD descriptor\n", stderr);
			return -1;
		}
		value = value * 10 + digit;
	}
	socket_fd = (int)value;
	if (getsockopt(socket_fd, SOL_SOCKET, SO_TYPE, &socket_type,
		       &socket_type_length) != 0 ||
	    (socket_type != SOCK_STREAM && socket_type != SOCK_DGRAM &&
	     socket_type != SOCK_SEQPACKET) ||
	    getsockopt(socket_fd, SOL_SOCKET, SO_PEERCRED, &peer, &length) != 0 ||
	    length != sizeof(peer) || peer.uid != uid) {
		fputs("fusermount3: _FUSE_COMMFD must be a FUSE socket owned by the calling user\n", stderr);
		return -1;
	}
	return socket_fd;
}

static int send_fuse_descriptor(int socket_fd, int fuse_fd)
{
	char byte = '\0';
	struct iovec iov = {.iov_base = &byte, .iov_len = sizeof(byte)};
	union {
		struct cmsghdr alignment;
		char bytes[CMSG_SPACE(sizeof(int))];
	} control;
	struct msghdr message;
	struct cmsghdr *cmsg;
	ssize_t sent;

	memset(&message, 0, sizeof(message));
	memset(&control, 0, sizeof(control));
	message.msg_iov = &iov;
	message.msg_iovlen = 1;
	message.msg_control = control.bytes;
	message.msg_controllen = sizeof(control.bytes);
	cmsg = CMSG_FIRSTHDR(&message);
	cmsg->cmsg_level = SOL_SOCKET;
	cmsg->cmsg_type = SCM_RIGHTS;
	cmsg->cmsg_len = CMSG_LEN(sizeof(int));
	memcpy(CMSG_DATA(cmsg), &fuse_fd, sizeof(fuse_fd));
	do {
		sent = sendmsg(socket_fd, &message, MSG_NOSIGNAL);
	} while (sent < 0 && errno == EINTR);
	return sent == 1 ? 0 : -1;
}

static void unmount_after_failure(void)
{
	if (umount2(IMMUTABLE_MOUNTPOINT, 0) != 0 &&
	    umount2(IMMUTABLE_MOUNTPOINT, MNT_DETACH) != 0)
		perror("fusermount3: could not remove the failed FUSE mount");
}

static int mount_immutable_directory(const struct mount_options *options,
				     int mountpoint_fd, int fuse_fd,
				     uid_t uid, gid_t gid, int socket_fd)
{
	char target[64];
	char mount_data[256];
	int length;
	unsigned long flags = MS_NODEV | MS_NOSUID;

	length = snprintf(target, sizeof(target), "/proc/self/fd/%d", mountpoint_fd);
	if (length < 0 || (size_t)length >= sizeof(target)) {
		fputs("fusermount3: invalid pinned mountpoint descriptor\n", stderr);
		return 1;
	}
	length = snprintf(mount_data, sizeof(mount_data),
			  "fd=%d,rootmode=040000,user_id=%lu,group_id=%lu,subtype=rclone-degoo",
			  fuse_fd, (unsigned long)uid, (unsigned long)gid);
	if (length < 0 || (size_t)length >= sizeof(mount_data)) {
		fputs("fusermount3: cannot construct FUSE mount options\n", stderr);
		return 1;
	}
	if (options->have_max_read) {
		int added = snprintf(mount_data + length, sizeof(mount_data) - (size_t)length,
				     ",max_read=%u", options->max_read);
		if (added < 0 || (size_t)length + (size_t)added >= sizeof(mount_data)) {
			fputs("fusermount3: cannot construct FUSE mount options\n", stderr);
			return 1;
		}
		length += added;
	}
	if (options->default_permissions) {
		int added = snprintf(mount_data + length, sizeof(mount_data) - (size_t)length,
				     ",default_permissions");
		if (added < 0 || (size_t)length + (size_t)added >= sizeof(mount_data)) {
			fputs("fusermount3: cannot construct FUSE mount options\n", stderr);
			return 1;
		}
		length += added;
	}
	if (options->read_only)
		flags |= MS_RDONLY;

	if (mount("rclone-degoo", target, "fuse", flags, mount_data) != 0) {
		perror("fusermount3: FUSE mount failed");
		return 1;
	}
	if (send_fuse_descriptor(socket_fd, fuse_fd) != 0) {
		perror("fusermount3: could not send the FUSE descriptor");
		unmount_after_failure();
		return 1;
	}
	return 0;
}

int main(int argc, char **argv)
{
	const char *target;
	const char *option_text;
	struct mount_options options;
	char parse_error[128] = "";
	uid_t uid = getuid();
	gid_t gid = getgid();
	int mountpoint_fd = -1;
	int fuse_fd = -1;
	int socket_fd = -1;
	int result = 1;

	if (is_unmount_request(argc, argv))
		return delegate_to_system_helper(argc, argv, false);
	target = find_target(argc, argv);
	if (target == NULL || strcmp(target, IMMUTABLE_MOUNTPOINT) != 0)
		return delegate_to_system_helper(argc, argv, true);
	if (decode_mount_request(argc, argv, &target, &option_text) != 0) {
		fputs("fusermount3: unsupported mount invocation for /mnt/degoo\n", stderr);
		return 1;
	}
	if (parse_options(option_text, &options, parse_error, sizeof(parse_error)) != 0) {
		fprintf(stderr, "fusermount3: %s\n", parse_error);
		return 1;
	}
	{
		int mountpoint_result = open_immutable_mountpoint(uid, &mountpoint_fd);
		if (mountpoint_result == MOUNTPOINT_DELEGATE)
			return delegate_to_system_helper(argc, argv, true);
		if (mountpoint_result == MOUNTPOINT_REJECT)
			goto out;
	}
	if (geteuid() != 0) {
		fputs("fusermount3: immutable mount helper must be installed setuid root\n", stderr);
		goto out;
	}
	fuse_fd = open_fuse_device();
	if (fuse_fd < 0)
		goto out;
	socket_fd = get_communication_socket(uid);
	if (socket_fd < 0)
		goto out;
	result = mount_immutable_directory(&options, mountpoint_fd, fuse_fd, uid, gid, socket_fd);

out:
	if (socket_fd >= 0)
		(void)close(socket_fd);
	if (fuse_fd >= 0)
		(void)close(fuse_fd);
	if (mountpoint_fd >= 0)
		(void)close(mountpoint_fd);
	return result;
}

#else

static int expect_result(const char *text, bool want_success,
			 bool want_read_only, bool want_default_permissions,
			 bool want_max_read, unsigned int want_max_read_value)
{
	struct mount_options options;
	char error[128] = "";
	int result = parse_options(text, &options, error, sizeof(error));

	if ((result == 0) != want_success) {
		fprintf(stderr, "unexpected parse result for %s: %s\n", text, error);
		return 1;
	}
	if (result == 0 && (options.read_only != want_read_only ||
			    options.default_permissions != want_default_permissions ||
			    options.have_max_read != want_max_read ||
			    (want_max_read && options.max_read != want_max_read_value))) {
		fprintf(stderr, "unexpected parsed option values for %s\n", text);
		return 1;
	}
	return 0;
}

int main(void)
{
	int failures = 0;

	failures += expect_result("fsname=device", true, false, false, false, 0);
	failures += expect_result("fsname=device,subtype=rclone,default_permissions",
				  true, false, true, false, 0);
	failures += expect_result("subtype=rclone", true, false, false, false, 0);
	failures += expect_result("ro", true, true, false, false, 0);
	failures += expect_result("default_permissions", true, false, true, false, 0);
	failures += expect_result("max_read=131072", true, false, false, true, 131072);
	failures += expect_result("fsname=device,subtype=rclone,max_read=131072",
				  true, false, false, true, 131072);
	failures += expect_result("fsname=rclone:remote\\,name,subtype=rclone,ro,default_permissions,max_read=131072",
				  true, true, true, true, 131072);
	failures += expect_result("max_read=999999999", true, false, false,
				  true, MAX_READ_LIMIT);
	failures += expect_result("", true, false, false, false, 0);
	failures += expect_result("allow_other", false, false, false, false, 0);
	failures += expect_result("allow_root", false, false, false, false, 0);
	failures += expect_result("user_id=0", false, false, false, false, 0);
	failures += expect_result("group_id=0", false, false, false, false, 0);
	failures += expect_result("fd=3", false, false, false, false, 0);
	failures += expect_result("rootmode=040000", false, false, false, false, 0);
	failures += expect_result("suid", false, false, false, false, 0);
	failures += expect_result("dev", false, false, false, false, 0);
	failures += expect_result("rw", false, false, false, false, 0);
	failures += expect_result("max_read=0", false, false, false, false, 0);
	failures += expect_result("max_read=12x", false, false, false, false, 0);
	failures += expect_result("ro,ro", false, false, false, false, 0);
	failures += expect_result("subtype=rclone\\q", false, false, false, false, 0);
	failures += expect_result("ro,", false, false, false, false, 0);
	if (failures != 0)
		return 1;
	puts("immutable FUSE helper parser tests passed");
	return 0;
}

#endif
