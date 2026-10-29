<?php

declare(strict_types=1);

namespace DoctrineMigrations;

use Doctrine\DBAL\Schema\Schema;
use Doctrine\Migrations\AbstractMigration;

final class Version20250101000000 extends AbstractMigration
{
    public function getDescription(): string
    {
        return 'Create orders';
    }

    public function up(Schema $schema): void
    {
        $this->addSql("CREATE TABLE orders (
            id SERIAL PRIMARY KEY,
            created_by VARCHAR(180) NOT NULL,
            status VARCHAR(32) NOT NULL DEFAULT 'new',
            legacy_note TEXT DEFAULT NULL,
            old_flag BOOLEAN DEFAULT NULL,
            total NUMERIC(12, 2) NOT NULL DEFAULT 0
        )");
    }

    public function down(Schema $schema): void
    {
        $this->addSql('DROP TABLE orders');
    }
}
