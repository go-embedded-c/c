#include <stdio.h>
#include <stdint.h>
#include <inttypes.h>

int main(void)
{
	printf("[%5d][%-5d][%05d][%+d]\n", 42, 42, 42, 42);
	printf("[%x][%X][%#x][%o][%#o]\n", 255, 255, 255, 8, 8);
	printf("[%u][%ld][%lld]\n", 4000000000u, -2147483647L - 1, -9223372036854775807LL - 1);
	printf("[%" PRId64 "][%" PRIu64 "]\n", INT64_MIN, UINT64_MAX);
	printf("[%c][%s][%10s][%-10s][%.3s]\n", 'z', "str", "right", "left", "truncated");
	printf("[%f][%.2f][%e][%g][%g]\n", 3.14159265358979, 2.5, 12345.6789, 0.0001, 1e20);
	printf("[%p]\n", (void *)0);
	printf("[%%][%5.1f][%-8.3e]\n", -0.05, 1.0);
	return 0;
}
