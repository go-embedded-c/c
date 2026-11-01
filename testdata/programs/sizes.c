/* The wasm32 ABI of clang: ILP32, 16-byte long double, signed char. */
#include <stdio.h>
#include <stddef.h>
#include <stdint.h>

struct s { char c; double d; short h; };
union u { char c; long long ll; };

int main(void)
{
	printf("char=%zu short=%zu int=%zu long=%zu llong=%zu\n",
	       sizeof(char), sizeof(short), sizeof(int), sizeof(long), sizeof(long long));
	printf("ptr=%zu float=%zu double=%zu ldouble=%zu\n",
	       sizeof(void *), sizeof(float), sizeof(double), sizeof(long double));
	printf("struct s: size=%zu off(d)=%zu off(h)=%zu\n",
	       sizeof(struct s), offsetof(struct s, d), offsetof(struct s, h));
	printf("union u: size=%zu\n", sizeof(union u));
	printf("size_t=%zu ptrdiff_t=%zu intmax_t=%zu wchar_t=%zu\n",
	       sizeof(size_t), sizeof(ptrdiff_t), sizeof(intmax_t), sizeof(wchar_t));
	printf("char is %s\n", (char)-1 < 0 ? "signed" : "unsigned");
	return 0;
}
