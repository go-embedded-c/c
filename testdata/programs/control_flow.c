/* goto, switch with fallthrough, and Duff's device: the structured-control-flow cases. */
#include <stdio.h>

static int dispatch(int n)
{
	int r = 0;
	switch (n) {
	case 0: r += 1;
	case 1: r += 10;
	case 2: r += 100; break;
	default: r = -1;
	}
	return r;
}

static void duff(char *to, const char *from, int count)
{
	int n = (count + 7) / 8;
	switch (count % 8) {
	case 0: do { *to++ = *from++;
	case 7: *to++ = *from++;
	case 6: *to++ = *from++;
	case 5: *to++ = *from++;
	case 4: *to++ = *from++;
	case 3: *to++ = *from++;
	case 2: *to++ = *from++;
	case 1: *to++ = *from++;
		} while (--n > 0);
	}
}

int main(void)
{
	char buf[32] = {0};
	int i = 0, sum = 0;
loop:
	if (i >= 5)
		goto done;
	sum += i++;
	goto loop;
done:
	printf("sum=%d\n", sum);
	printf("%d %d %d %d\n", dispatch(0), dispatch(1), dispatch(2), dispatch(9));
	duff(buf, "the quick brown fox jumps", 25);
	printf("%s\n", buf);
	return 0;
}
