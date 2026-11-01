/* libm results are musl's, the same on every host. */
#include <stdio.h>
#include <math.h>

int main(void)
{
	volatile double x = 2.0;
	printf("%.17g\n", sqrt(x));
	printf("%.17g\n", sin(1.0));
	printf("%.17g\n", cos(1.0));
	printf("%.17g\n", exp(1.0));
	printf("%.17g\n", log(10.0));
	printf("%.17g\n", pow(x, 0.5));
	printf("%.17g\n", atan2(1.0, 2.0));
	printf("%.17g\n", fmod(10.5, 3.0));
	printf("%d %d %d\n", isnan(NAN), isinf(INFINITY), signbit(-0.0) != 0);
	printf("%.17g\n", (double)(float)0.1);
	return 0;
}
