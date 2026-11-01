/* long double is IEEE binary128 on wasm32 (software arithmetic). */
#include <stdio.h>
#include <float.h>

int main(void)
{
	volatile long double a = 1.0L, b = 3.0L;
	long double q = a / b;
	printf("%d %d\n", LDBL_MANT_DIG, LDBL_MAX_EXP);
	printf("%.30Lg\n", q);
	printf("%d\n", (double)q == 1.0 / 3.0);
	printf("%.20Lg\n", (long double)(1ULL << 63) * 4);
	return 0;
}
