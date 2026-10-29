from django.db import migrations, models


class Migration(migrations.Migration):
    dependencies = [("shop", "0001_initial")]

    operations = [
        migrations.RemoveField(model_name="order", name="created_by"),
        migrations.AddIndex(
            model_name="order",
            index=models.Index(fields=["status"], name="orders_status_idx"),
        ),
        migrations.RunSQL(
            sql="CREATE TABLE order_audit (id bigserial PRIMARY KEY, order_id bigint NOT NULL)",
            reverse_sql="DROP TABLE order_audit",
        ),
    ]
