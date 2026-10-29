from django.contrib.postgres.operations import AddIndexConcurrently
from django.db import migrations, models


class Migration(migrations.Migration):
    atomic = False

    dependencies = [("shop", "0001_initial")]

    operations = [
        AddIndexConcurrently(
            model_name="order",
            index=models.Index(fields=["status"], name="orders_status_idx"),
        ),
    ]
