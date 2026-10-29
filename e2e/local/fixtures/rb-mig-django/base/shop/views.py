from django.http import JsonResponse

from shop.models import Order


def my_orders(request):
    orders = Order.objects.filter(created_by=request.user.email).order_by("-id")
    return JsonResponse(
        [{"id": o.id, "status": o.status, "total": str(o.total), "promo": o.promo_code} for o in orders],
        safe=False,
    )
