#include <stdarg.h>
#include <stdio.h>

static double mean(int n, ...)
{
	va_list ap;
	double s = 0;
	va_start(ap, n);
	for (int i = 0; i < n; i++)
		s += va_arg(ap, double);
	va_end(ap);
	return s / n;
}

static long long total(int n, ...)
{
	va_list ap, copy;
	long long s = 0;
	va_start(ap, n);
	va_copy(copy, ap);
	for (int i = 0; i < n; i++)
		s += va_arg(ap, long long);
	for (int i = 0; i < n; i++)
		s += va_arg(copy, long long);
	va_end(ap);
	va_end(copy);
	return s;
}

int main(void)
{
	printf("%g\n", mean(4, 1.0, 2.0, 3.0, 4.5));
	printf("%lld\n", total(3, 10000000000LL, 2LL, 3LL));
	return 0;
}
