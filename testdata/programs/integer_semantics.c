#include <stdio.h>
#include <limits.h>

int main(void)
{
	volatile int i = -7;
	volatile unsigned u = 3;
	printf("%d %d\n", i / 2, i % 2);
	printf("%d\n", i >> 1);
	printf("%u\n", (unsigned)i >> 28);
	printf("%d\n", (int)(unsigned char)300);
	printf("%d\n", (signed char)200);
	printf("%u\n", u - 4);
	printf("%d %d\n", INT_MAX, INT_MIN);
	printf("%lld\n", (long long)INT_MAX * INT_MAX);
	printf("%d\n", -i < u);
	printf("%d\n", sizeof(int) - 5 > 0);
	return 0;
}
