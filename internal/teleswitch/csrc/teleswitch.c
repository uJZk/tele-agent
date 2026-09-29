/*
 * teleswitch moves the Claude process from the view it was started in to
 * the remote view before Claude's main runs (docs/filesystem.md
 * "teleswitch", "命名空间的构建").
 *
 * It is preloaded with LD_PRELOAD. Its constructor runs after the dynamic
 * linker has mapped every library Claude needs, while the process is still
 * single-threaded, which setns(CLONE_NEWNS) requires. It does not link
 * libc (no DT_NEEDED, raw system calls only, no allocation), so it works
 * with glibc and musl builds of Claude alike
 * (docs/coding-standards.md "C 代码（teleswitch）").
 *
 * Every step must succeed: on any failure it writes one line to
 * TS_DIAG_FD and exits with TS_EXIT_CODE, because a Claude that went on in
 * the view it was started in would take local files for remote ones. The
 * constants are shared with internal/teleswitch/teleswitch.go, whose tests
 * check that both sides agree.
 */
#include <asm/unistd.h>
#include <linux/capability.h>
#include <linux/prctl.h>
#include <linux/sched.h>

/* Shared with Go: internal/teleswitch/teleswitch.go. */
#define TS_ENV_FD "TELE_SWITCH_FD"   /* the remote view's mount namespace fd */
#define TS_ENV_DIR "TELE_SWITCH_DIR" /* the working directory in that view */
#define TS_ENV_PREFIX "TELE_SWITCH_" /* removed from the environment */
#define TS_ENV_CHECK "TELE_SESSION"  /* must exist in the remote view */
#define TS_EXIT_CODE 121             /* exit status of a failed switch */
#define TS_DIAG_FD 2                 /* where the diagnostic line goes */

typedef unsigned long ts_word;

/* The environment Claude will read; glibc and musl both export it. The
 * reference is weak and resolved at load time, which needs no DT_NEEDED.
 * glibc also passes it to constructors, which covers a missing symbol. */
extern char **__environ __attribute__((weak));

/* ts_syscall5 makes a system call with five arguments; unused ones must
 * be passed as 0, because some calls (prctl) reject garbage in them. */
#if defined(__x86_64__)
static long ts_syscall5(long n, long a, long b, long c, long d, long e) {
	long ret;
	register long r10 __asm__("r10") = d;
	register long r8 __asm__("r8") = e;
	__asm__ volatile("syscall"
			 : "=a"(ret)
			 : "a"(n), "D"(a), "S"(b), "d"(c), "r"(r10), "r"(r8)
			 : "rcx", "r11", "memory");
	return ret;
}
#elif defined(__aarch64__)
static long ts_syscall5(long n, long a, long b, long c, long d, long e) {
	register long x8 __asm__("x8") = n;
	register long x0 __asm__("x0") = a;
	register long x1 __asm__("x1") = b;
	register long x2 __asm__("x2") = c;
	register long x3 __asm__("x3") = d;
	register long x4 __asm__("x4") = e;
	__asm__ volatile("svc 0"
			 : "+r"(x0)
			 : "r"(x8), "r"(x1), "r"(x2), "r"(x3), "r"(x4)
			 : "memory");
	return x0;
}
#else
#error "teleswitch: unsupported architecture"
#endif

static long ts_syscall(long n, long a, long b, long c, long d) {
	return ts_syscall5(n, a, b, c, d, 0);
}

static unsigned long ts_strlen(const char *s) {
	unsigned long n = 0;
	while (s[n])
		n++;
	return n;
}

/* ts_value returns the value of entry e if it is "name=...", else 0. */
static const char *ts_value(const char *e, const char *name) {
	while (*name) {
		if (*e++ != *name++)
			return 0;
	}
	return *e == '=' ? e + 1 : 0;
}

static int ts_has_prefix(const char *e, const char *prefix) {
	while (*prefix) {
		if (*e++ != *prefix++)
			return 0;
	}
	return 1;
}

static void ts_write(const char *s) {
	unsigned long n = ts_strlen(s);
	while (n > 0) {
		long w = ts_syscall(__NR_write, TS_DIAG_FD, (long)s, (long)n, 0);
		if (w == -4 /* EINTR */)
			continue;
		if (w <= 0)
			return;
		s += w;
		n -= (unsigned long)w;
	}
}

/* ts_fail reports a failed step with its errno and ends the process. */
__attribute__((noreturn)) static void ts_fail(const char *step, long err) {
	char num[24];
	int i = sizeof num;
	unsigned long e = (unsigned long)(err < 0 ? -err : err);
	num[--i] = 0;
	num[--i] = '\n';
	do {
		num[--i] = (char)('0' + e % 10);
		e /= 10;
	} while (e && i > 0);
	ts_write("tele: teleswitch: cannot switch to the remote view: ");
	ts_write(step);
	ts_write(" failed, errno ");
	ts_write(num + i);
	for (;;)
		ts_syscall(__NR_exit_group, TS_EXIT_CODE, 0, 0, 0);
}

static long ts_parse_fd(const char *s) {
	long v = 0;
	if (!s || !*s)
		return -1;
	for (; *s; s++) {
		if (*s < '0' || *s > '9' || v > 1000000)
			return -1;
		v = v * 10 + (*s - '0');
	}
	return v;
}

/* ts_scrub removes LD_PRELOAD and the TELE_SWITCH_ variables from env in
 * place, so that nothing Claude starts inherits them. Their slots become
 * empty strings, which no lookup matches and which are not variables,
 * instead of being compacted away: the array must keep its length,
 * because runtimes find the auxiliary vector by walking past the
 * environment's terminating NULL on the initial stack. */
static void ts_scrub(char **env) {
	for (char **p = env; *p; p++) {
		if (ts_has_prefix(*p, "LD_PRELOAD=") || ts_has_prefix(*p, TS_ENV_PREFIX))
			*p += ts_strlen(*p);
	}
}

static void ts_switch(char **env) {
	const char *fdv = 0, *dir = 0, *check = 0;
	long fd, r;

	if (!env)
		ts_fail("finding the environment", 22 /* EINVAL */);
	for (char **p = env; *p; p++) {
		const char *v;
		if ((v = ts_value(*p, TS_ENV_FD)))
			fdv = v;
		else if ((v = ts_value(*p, TS_ENV_DIR)))
			dir = v;
		else if ((v = ts_value(*p, TS_ENV_CHECK)))
			check = v;
	}
	fd = ts_parse_fd(fdv);
	if (fd < 0 || !dir || !*dir || !check || !*check)
		ts_fail("reading " TS_ENV_FD ", " TS_ENV_DIR " and " TS_ENV_CHECK, 22);

	r = ts_syscall(__NR_setns, fd, CLONE_NEWNS, 0, 0);
	if (r < 0)
		ts_fail("setns", r);
	r = ts_syscall(__NR_chdir, (long)dir, 0, 0, 0);
	if (r < 0)
		ts_fail("chdir to the working directory", r);
	/* The session directory exists only in the remote view: seeing it
	 * confirms the switch. */
#ifdef __NR_access
	r = ts_syscall(__NR_access, (long)check, 0 /* F_OK */, 0, 0);
#else
	r = ts_syscall(__NR_faccessat, -100 /* AT_FDCWD */, (long)check, 0, 0);
#endif
	if (r < 0)
		ts_fail("finding the session directory in the remote view", r);
	r = ts_syscall(__NR_close, fd, 0, 0, 0);
	if (r < 0)
		ts_fail("close", r);
	ts_scrub(env);

	r = ts_syscall(__NR_prctl, PR_CAP_AMBIENT, PR_CAP_AMBIENT_CLEAR_ALL, 0, 0);
	if (r < 0)
		ts_fail("clearing ambient capabilities", r);
	struct __user_cap_header_struct hdr = {_LINUX_CAPABILITY_VERSION_3, 0};
	struct __user_cap_data_struct data[_LINUX_CAPABILITY_U32S_3];
	for (int i = 0; i < _LINUX_CAPABILITY_U32S_3; i++)
		data[i].effective = data[i].permitted = data[i].inheritable = 0;
	r = ts_syscall(__NR_capset, (long)&hdr, (long)data, 0, 0);
	if (r < 0)
		ts_fail("capset", r);
}

__attribute__((constructor)) static void ts_init(int argc, char **argv, char **envp) {
	(void)argc;
	(void)argv;
	char **env = envp;
	if (&__environ && __environ)
		env = __environ;
	ts_switch(env);
}
