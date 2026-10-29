from django.db import migrations, models


class Migration(migrations.Migration):
    initial = True

    dependencies = []

    operations = [
        migrations.CreateModel(
            name="Order",
            fields=[
                ("id", models.BigAutoField(auto_created=True, primary_key=True, serialize=False)),
                ("created_by", models.EmailField(max_length=180)),
                ("status", models.CharField(default="new", max_length=32)),
                ("total", models.DecimalField(decimal_places=2, default=0, max_digits=12)),
                ("promo_code", models.CharField(blank=True, default="", max_length=32)),
                ("created_at", models.DateTimeField(auto_now_add=True)),
            ],
            options={"db_table": "orders"},
        ),
    ]
